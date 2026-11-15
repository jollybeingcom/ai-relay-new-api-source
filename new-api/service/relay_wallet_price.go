package service

import (
	"encoding/json"
	"errors"
	"regexp"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

var ErrRelayUsageUnknown = errors.New("relay usage is missing, inconsistent or outside the reserved bounds")
var relayRatePattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,15})(\.[0-9]{1,12})?$`)

// Rates are existing New API quota units per million tokens, not currency.
// Only trusted server configuration may construct this snapshot. ContextTokens
// must be a verified provider limit; InputLimit is a hard bound, not an estimate.
// This initial contract supports text input, cache reads and total output only.
type RelayTextPrice struct {
	ChannelID     int    `json:"channel_id"`
	Schema        string `json:"schema"`
	Version       string `json:"version"`
	Model         string `json:"model"`
	Unit          string `json:"unit"`
	InputRate     string `json:"input_rate"`
	CacheReadRate string `json:"cache_read_rate"`
	OutputRate    string `json:"output_rate"`
	ContextTokens int    `json:"context_tokens"`
	InputLimit    int    `json:"input_limit"`
	OutputLimit   int    `json:"output_limit"`
}

func (price RelayTextPrice) rates() ([3]decimal.Decimal, error) {
	var rates [3]decimal.Decimal
	if price.Schema != "text-v1" || price.Unit != "new-api-quota" || len(price.Version) == 0 || len(price.Version) > 100 || len(price.Model) == 0 || len(price.Model) > 200 ||
		price.ContextTokens <= 0 || price.ContextTokens > common.MaxQuota || price.InputLimit <= 0 || price.OutputLimit <= 0 ||
		price.InputLimit > price.ContextTokens || price.OutputLimit > price.ContextTokens-price.InputLimit {
		return rates, model.ErrRelayInput
	}
	for i, raw := range []string{price.InputRate, price.CacheReadRate, price.OutputRate} {
		if !relayRatePattern.MatchString(raw) {
			return rates, model.ErrRelayInput
		}
		value, err := decimal.NewFromString(raw)
		if err != nil {
			return rates, model.ErrRelayInput
		}
		rates[i] = value
	}
	return rates, nil
}

// Reserve the most expensive possible input category and maximum total output.
// Round up once after summing; reject saturation rather than reserve too little.
func (price RelayTextPrice) Reservation() (int, error) {
	rates, err := price.rates()
	if err != nil {
		return 0, err
	}
	inputRate := decimal.Max(rates[0], rates[1])
	amount := inputRate.Mul(decimal.NewFromInt(int64(price.InputLimit))).Add(rates[2].Mul(decimal.NewFromInt(int64(price.OutputLimit)))).Shift(-6)
	quota, err := common.QuotaFromDecimalStrict(amount.Ceil())
	if err != nil {
		return 0, err
	}
	if quota <= 0 {
		return 0, model.ErrRelayInput
	}
	return quota, nil
}

// Pointers distinguish an explicit zero from absent upstream facts. The caller
// must obtain these from a completed upstream response, never a client payload
// or local tokenizer. Output includes reasoning tokens; cache is part of input.
type RelayTextUsage struct {
	Model     string
	Complete  bool
	Input     *int
	CacheRead *int
	Output    *int
	Total     *int
}

// Parse only the original upstream usage object for a text completion. The
// transport must separately establish completion and model identity. Never feed
// a re-marshaled dto.Usage here: its non-pointer fields erase missing facts.
func ParseRelayTextUsage(raw []byte, modelName string, complete bool) (RelayTextUsage, error) {
	facts := RelayTextUsage{Model: modelName, Complete: complete}
	if !complete || len(raw) == 0 || len(raw) > 16384 {
		return facts, ErrRelayUsageUnknown
	}
	var fields map[string]json.RawMessage
	if err := common.Unmarshal(raw, &fields); err != nil || len(fields) == 0 {
		return facts, ErrRelayUsageUnknown
	}
	var hit, miss, cached, reasoning *int
	for name, value := range fields {
		var destination **int
		switch name {
		case "prompt_tokens":
			destination = &facts.Input
		case "completion_tokens":
			destination = &facts.Output
		case "total_tokens":
			destination = &facts.Total
		case "prompt_cache_hit_tokens":
			destination = &hit
		case "prompt_cache_miss_tokens":
			destination = &miss
		case "prompt_tokens_details", "completion_tokens_details":
			var details map[string]json.RawMessage
			if err := common.Unmarshal(value, &details); err != nil {
				return facts, ErrRelayUsageUnknown
			}
			for key, count := range details {
				var target **int
				if name == "prompt_tokens_details" && key == "cached_tokens" {
					target = &cached
				} else if name == "completion_tokens_details" && key == "reasoning_tokens" {
					target = &reasoning
				} else {
					return facts, ErrRelayUsageUnknown
				}
				if err := common.Unmarshal(count, target); err != nil || *target == nil || **target < 0 || **target > common.MaxQuota {
					return facts, ErrRelayUsageUnknown
				}
			}
			continue
		default:
			return facts, ErrRelayUsageUnknown // Unsupported billable modalities must not disappear.
		}
		if err := common.Unmarshal(value, destination); err != nil || *destination == nil || **destination < 0 || **destination > common.MaxQuota {
			return facts, ErrRelayUsageUnknown
		}
	}
	if hit != nil && cached != nil && *hit != *cached {
		return facts, ErrRelayUsageUnknown
	}
	facts.CacheRead = cached
	if facts.CacheRead == nil {
		facts.CacheRead = hit
	}
	if facts.Input == nil || facts.Output == nil || facts.Total == nil || facts.CacheRead == nil || *facts.CacheRead > *facts.Input ||
		int64(*facts.Total) != int64(*facts.Input)+int64(*facts.Output) {
		return facts, ErrRelayUsageUnknown
	}
	if miss != nil && *miss != *facts.Input-*facts.CacheRead {
		return facts, ErrRelayUsageUnknown
	}
	if reasoning != nil && *reasoning > *facts.Output {
		return facts, ErrRelayUsageUnknown
	}
	return facts, nil
}

func (session *RelayWalletSession) SettleRawTextUsage(raw []byte, modelName string, complete bool) error {
	facts, err := ParseRelayTextUsage(raw, modelName, complete)
	if err != nil {
		if failErr := session.Fail(); failErr != nil {
			return errors.Join(err, failErr)
		}
		return err
	}
	return session.SettleTextUsage(facts)
}

func (price RelayTextPrice) Charge(usage RelayTextUsage) (int, error) {
	rates, err := price.rates()
	if err != nil {
		return 0, err
	}
	if !usage.Complete || usage.Model != price.Model || usage.Input == nil || usage.CacheRead == nil || usage.Output == nil || usage.Total == nil {
		return 0, ErrRelayUsageUnknown
	}
	input, cache, output := *usage.Input, *usage.CacheRead, *usage.Output
	if input < 0 || input > price.InputLimit || cache < 0 || cache > input || output < 0 || output > price.OutputLimit || *usage.Total != input+output {
		return 0, ErrRelayUsageUnknown
	}
	amount := rates[0].Mul(decimal.NewFromInt(int64(input - cache))).Add(rates[1].Mul(decimal.NewFromInt(int64(cache)))).Add(rates[2].Mul(decimal.NewFromInt(int64(output)))).Shift(-6)
	return common.QuotaFromDecimalStrict(amount)
}

// Replace caller-supplied price fields with the validated, serialized snapshot.
// Request identity and payload hash still come from authenticated server code.
func NewPricedRelayWalletSession(db *gorm.DB, intent model.RelayWalletRequest, price RelayTextPrice) (*RelayWalletSession, error) {
	if basis := intent.CostBasis; basis != nil {
		if basis.ChannelID != price.ChannelID || basis.Model != price.Model || basis.InputLimit != price.InputLimit || basis.OutputLimit != price.OutputLimit {
			return nil, model.ErrRelayConflict
		}
	}
	if price.ChannelID <= 0 {
		return nil, model.ErrRelayInput
	}
	quota, err := price.Reservation()
	if err != nil {
		return nil, err
	}
	snapshot, err := common.Marshal(price)
	if err != nil {
		return nil, err
	}
	intent.Reserved, intent.PriceVersion, intent.PriceSnapshot = int64(quota), price.Version, string(snapshot)
	return NewRelayWalletSession(db, intent)
}

// Re-read the immutable price from the durable intent, so restart or admin price
// changes cannot reprice an in-flight request. Bad facts hold funds as unknown.
func (session *RelayWalletSession) SettleTextUsage(usage RelayTextUsage) error {
	request, err := session.load()
	if err != nil {
		return err
	}
	if request.State != "submitted" && request.State != "unknown" && request.State != "settled" {
		return model.ErrRelayState
	}
	var price RelayTextPrice
	err = common.UnmarshalJsonStr(request.PriceSnapshot, &price)
	var reserve, charge int
	if err == nil {
		reserve, err = price.Reservation()
	}
	if err == nil && (price.Version != request.PriceVersion || int64(reserve) != request.Reserved) {
		err = model.ErrRelayConflict
	}
	if err == nil {
		charge, err = price.Charge(usage)
	}
	if err != nil {
		if failErr := session.Fail(); failErr != nil {
			return errors.Join(err, failErr)
		}
		return err
	}
	return model.SettleRelayWalletReceipt(session.db, model.RelayWalletReceipt{
		RequestID: request.ID, UserID: request.UserID, TokenID: request.TokenID, ChannelID: price.ChannelID,
		Model: price.Model, PriceVersion: price.Version, Input: *usage.Input, CacheRead: *usage.CacheRead, Output: *usage.Output, Actual: int64(charge),
	})
}
