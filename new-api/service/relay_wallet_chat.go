package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// RelayQuotaPerCNY fixes the paid relay's money unit: 1 CNY = 1,000,000 quota.
// Common users had no funds when it was adopted, so no balance conversion ran.
const RelayQuotaPerCNY = 1000000

// RelayWalletMandatoryKey marks a common user whose paid relay must use the
// durable wallet. Such a request never falls back to the stock billing chain.
const RelayWalletMandatoryKey = "relay_wallet_mandatory"

const (
	relayWalletRequestHeader = "X-Relay-Request-Id"
	relayWalletBodyLimit     = 1 << 20
	relayWalletResponseLimit = 16 << 20
	relayWalletLineLimit     = 1 << 20
)

var (
	errRelayWalletOutbound = errors.New("outbound request violates the reserved price bounds")
	errRelayWalletDisabled = errors.New("paid relay is unavailable: durable wallet configuration is incomplete")
)

func RelayWalletBillingEnabled() bool {
	return common.GetEnvOrDefaultBool("RELAY_WALLET_BILLING", false)
}

// AllowRelayWalletRoute limits enabled common users to the one transport whose
// usage, output bound and completion are verified. Operators keep stock routes.
func AllowRelayWalletRoute(c *gin.Context, role int) bool {
	if !RelayWalletBillingEnabled() || role != common.RoleCommonUser {
		return true
	}
	c.Set(RelayWalletMandatoryKey, true)
	path := c.Request.URL.Path
	switch c.Request.Method {
	case http.MethodPost:
		return path == "/v1/chat/completions"
	case http.MethodGet:
		return path == "/v1/models" || strings.HasPrefix(path, "/v1/models/") || strings.HasPrefix(path, "/dashboard/billing/")
	}
	return false
}

func relayWalletMandatory(c *gin.Context) bool {
	if !RelayWalletBillingEnabled() {
		return false
	}
	// Token auth sets the flag; session-authenticated playground users set role.
	return c.GetBool(RelayWalletMandatoryKey) || c.GetInt("role") == common.RoleCommonUser
}

// A route binds one channel and client-visible model to an immutable price.
// OutputCap documents the operator's verification that max_tokens bounds the
// total output for this model, including any reasoning. Nothing else is trusted.
// Upstream rates are the provider's verified cost in the same quota units per
// million tokens. They stay server-side and only guard against selling at a loss.
type relayWalletChatRoute struct {
	RelayTextPrice
	RequestModel string `json:"request_model"`
	OutputCap    string `json:"output_cap"`
	// "none": verified non-reasoning model. "disabled": a reasoning model whose
	// max_tokens excludes thinking; the server forces enable_thinking=false.
	Thinking              string `json:"thinking"`
	UpstreamInputRate     string `json:"upstream_input_rate"`
	UpstreamCacheReadRate string `json:"upstream_cache_read_rate"`
	UpstreamOutputRate    string `json:"upstream_output_rate"`
}

// RelayWalletPublicPrice is the published, user-facing view of one route.
type RelayWalletPublicPrice struct {
	Model            string `json:"model"`
	UpstreamModel    string `json:"upstream_model"`
	Version          string `json:"version"`
	InputPerMillion  string `json:"input_cny_per_million"`
	CachePerMillion  string `json:"cache_read_cny_per_million"`
	OutputPerMillion string `json:"output_cny_per_million"`
	InputLimit       int    `json:"input_limit"`
	OutputLimit      int    `json:"output_limit"`
	MaxCharge        string `json:"max_charge_cny"`
}

func relayWalletMaxUpstreamCost() int {
	cost := common.GetEnvOrDefault("RELAY_WALLET_MAX_UPSTREAM_COST", RelayQuotaPerCNY)
	if cost < 1 || cost > 100*RelayQuotaPerCNY {
		return RelayQuotaPerCNY
	}
	return cost
}

// The sale rate must cover cost in every category, and one request's worst-case
// provider cost must stay within the configured cap (default 1 CNY).
func (route relayWalletChatRoute) validateCost(rates [3]decimal.Decimal) error {
	var upstream [3]decimal.Decimal
	for i, raw := range []string{route.UpstreamInputRate, route.UpstreamCacheReadRate, route.UpstreamOutputRate} {
		if !relayRatePattern.MatchString(raw) {
			return model.ErrRelayInput
		}
		value, err := decimal.NewFromString(raw)
		if err != nil || value.GreaterThan(rates[i]) {
			return model.ErrRelayInput
		}
		upstream[i] = value
	}
	worst := decimal.Max(upstream[0], upstream[1]).Mul(decimal.NewFromInt(int64(route.InputLimit))).
		Add(upstream[2].Mul(decimal.NewFromInt(int64(route.OutputLimit)))).Shift(-6)
	if worst.GreaterThan(decimal.NewFromInt(int64(relayWalletMaxUpstreamCost()))) {
		return model.ErrRelayInput
	}
	return nil
}

func relayQuotaToCNY(value decimal.Decimal) string {
	return value.Div(decimal.NewFromInt(RelayQuotaPerCNY)).String()
}

func PublishedRelayWalletPrices() ([]RelayWalletPublicPrice, error) {
	if !RelayWalletBillingEnabled() {
		return []RelayWalletPublicPrice{}, nil
	}
	routes, err := activeRelayWalletRoutes()
	if err != nil {
		return nil, err
	}
	prices := make([]RelayWalletPublicPrice, 0, len(routes))
	for _, route := range routes {
		rates, err := route.rates()
		if err != nil {
			return nil, err
		}
		reserve, err := route.Reservation()
		if err != nil {
			return nil, err
		}
		prices = append(prices, RelayWalletPublicPrice{Model: route.RequestModel, UpstreamModel: route.Model, Version: route.Version,
			InputPerMillion: relayQuotaToCNY(rates[0]), CachePerMillion: relayQuotaToCNY(rates[1]), OutputPerMillion: relayQuotaToCNY(rates[2]),
			InputLimit: route.InputLimit, OutputLimit: route.OutputLimit, MaxCharge: relayQuotaToCNY(decimal.NewFromInt(int64(reserve)))})
	}
	sort.Slice(prices, func(i, j int) bool { return prices[i].Model < prices[j].Model })
	return prices, nil
}

type relayWalletRouteTable struct {
	routes map[string]relayWalletChatRoute
	err    error
}

var relayWalletRoutes = struct {
	sync.Mutex
	byPath map[string]relayWalletRouteTable
}{byPath: map[string]relayWalletRouteTable{}}

func relayWalletRouteKey(channelID int, requestModel string) string {
	return strconv.Itoa(channelID) + "\x00" + requestModel
}

// The price file loads once per process and is the base table. With
// RELAY_POLICY_VERSIONS on, published versions (relay_policy.go) replace a
// route from their effective time, without a restart. In-flight requests
// always settle from their stored snapshot.
func loadRelayWalletRoutes() (map[string]relayWalletChatRoute, error) {
	path := os.Getenv("RELAY_WALLET_PRICE_FILE")
	if path == "" {
		return nil, errRelayWalletDisabled
	}
	relayWalletRoutes.Lock()
	defer relayWalletRoutes.Unlock()
	if table, ok := relayWalletRoutes.byPath[path]; ok {
		return table.routes, table.err
	}
	routes, err := parseRelayWalletRoutes(path)
	relayWalletRoutes.byPath[path] = relayWalletRouteTable{routes: routes, err: err}
	return routes, err
}

// validate applies every rule a route must satisfy before it may price a
// request, for the price file and for a published version alike.
func (route relayWalletChatRoute) validate() error {
	if route.ChannelID <= 0 || route.RequestModel == "" || len(route.RequestModel) > 200 || route.OutputCap != "max_tokens_total" ||
		route.Thinking != "none" && route.Thinking != "disabled" {
		return model.ErrRelayInput
	}
	if _, err := route.Reservation(); err != nil {
		return err
	}
	rates, err := route.rates()
	if err != nil {
		return err
	}
	return route.validateCost(rates)
}

func parseRelayWalletRoutes(path string) (map[string]relayWalletChatRoute, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<20 {
		return nil, model.ErrRelayInput
	}
	var config struct {
		Routes []relayWalletChatRoute `json:"routes"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return nil, err
	}
	if len(config.Routes) == 0 || len(config.Routes) > 100 {
		return nil, model.ErrRelayInput
	}
	routes := make(map[string]relayWalletChatRoute, len(config.Routes))
	for _, route := range config.Routes {
		if err := route.validate(); err != nil {
			return nil, err
		}
		key := relayWalletRouteKey(route.ChannelID, route.RequestModel)
		if _, duplicate := routes[key]; duplicate {
			return nil, model.ErrRelayConflict
		}
		routes[key] = route
	}
	return routes, nil
}

func relayWalletUpstreamTimeout() time.Duration {
	seconds := common.GetEnvOrDefault("RELAY_WALLET_UPSTREAM_TIMEOUT_SECONDS", 600)
	if seconds < 30 || seconds > 3600 {
		seconds = 600
	}
	return time.Duration(seconds) * time.Second
}

func relayWalletReject(err error, code types.ErrorCode, status int) *types.NewAPIError {
	return types.NewErrorWithStatusCode(err, code, status, types.ErrOptionWithSkipRetry())
}

// BeginRelayWalletChat replaces stock pre-consumption for mandatory users. It
// returns handled=false only for callers outside the durable wallet policy.
func BeginRelayWalletChat(c *gin.Context, info *relaycommon.RelayInfo) (bool, *types.NewAPIError) {
	if !relayWalletMandatory(c) {
		return false, nil
	}
	if !common.GetEnvOrDefaultBool("RELAY_WALLET_SCHEMA", false) {
		return true, relayWalletReject(errRelayWalletDisabled, types.ErrorCode("relay_wallet_unavailable"), http.StatusServiceUnavailable)
	}
	routes, err := activeRelayWalletRoutes()
	if err != nil {
		common.SysError(common.LogText("relay wallet price table unavailable: %s", err.Error()))
		return true, relayWalletReject(errRelayWalletDisabled, types.ErrorCode("relay_wallet_unavailable"), http.StatusServiceUnavailable)
	}
	request, ok := info.Request.(*dto.GeneralOpenAIRequest)
	if !ok || info.RelayFormat != types.RelayFormatOpenAI || info.RelayMode != relayconstant.RelayModeChatCompletions {
		return true, relayWalletReject(errors.New("paid relay only supports text chat completions"), types.ErrorCodeInvalidRequest, http.StatusBadRequest)
	}
	if info.UserId <= 0 || info.TokenId <= 0 || info.TokenUnlimited {
		return true, relayWalletReject(errors.New("paid relay requires a finite user token"), types.ErrorCodeAccessDenied, http.StatusForbidden)
	}
	route, ok := routes[relayWalletRouteKey(c.GetInt("channel_id"), info.OriginModelName)]
	if !ok {
		return true, relayWalletReject(errors.New("model has no published durable price"), types.ErrorCodeModelPriceError, http.StatusBadRequest)
	}
	if request.N != nil && *request.N != 1 || len(request.Tools) > 0 || len(request.Functions) > 0 || len(request.Modalities) > 0 ||
		len(request.Audio) > 0 || len(request.Prediction) > 0 || request.Prefix != nil || request.Suffix != nil {
		return true, relayWalletReject(errors.New("paid relay only supports single text responses"), types.ErrorCodeInvalidRequest, http.StatusBadRequest)
	}
	stream := request.Stream != nil && *request.Stream
	if stream != info.IsStream {
		return true, relayWalletReject(errors.New("inconsistent stream mode"), types.ErrorCodeInvalidRequest, http.StatusBadRequest)
	}

	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return true, relayWalletReject(err, types.ErrorCodeReadRequestBodyFailed, http.StatusBadRequest)
	}
	body, err := storage.Bytes()
	if err != nil || len(body) > relayWalletBodyLimit {
		return true, relayWalletReject(errors.New("request body unavailable or too large"), types.ErrorCodeReadRequestBodyFailed, http.StatusRequestEntityTooLarge)
	}
	// A local estimate only rejects early. Settlement still requires the trusted
	// upstream count to stay within InputLimit, or the funds stay held as unknown.
	meta := request.GetTokenCountMeta()
	if meta == nil || len(meta.Files) > 0 {
		return true, relayWalletReject(errors.New("paid relay only supports text input"), types.ErrorCodeInvalidRequest, http.StatusBadRequest)
	}
	tokens, err := CountRequestToken(c, meta, info)
	if err != nil {
		return true, relayWalletReject(err, types.ErrorCodeCountTokenFailed, http.StatusBadRequest)
	}
	if tokens > route.InputLimit {
		return true, relayWalletReject(errors.New("input exceeds the published paid relay limit"), types.ErrorCodeInvalidRequest, http.StatusRequestEntityTooLarge)
	}
	info.SetEstimatePromptTokens(tokens)

	price := route.RelayTextPrice
	requested := max(valueOrZero(request.MaxTokens), valueOrZero(request.MaxCompletionTokens))
	if requested > 0 && requested < uint(price.OutputLimit) {
		price.OutputLimit = int(requested) // A smaller client cap lowers the hold.
	}
	limit := uint(price.OutputLimit)
	request.MaxTokens, request.MaxCompletionTokens = &limit, nil
	if stream {
		request.StreamOptions = &dto.StreamOptions{IncludeUsage: true}
	}
	if route.Thinking == "disabled" {
		request.EnableThinking = json.RawMessage("false")
	}

	id := strings.ToLower(c.GetHeader(relayWalletRequestHeader))
	if id == "" {
		id = uuid.NewString()
	} else if parsed, err := uuid.Parse(id); err != nil || parsed.Version() != 4 || parsed.String() != id {
		return true, relayWalletReject(errors.New("invalid relay request id"), types.ErrorCodeInvalidRequest, http.StatusBadRequest)
	}
	digest := sha256.Sum256(body)
	session, err := NewPricedRelayWalletSession(model.DB, model.RelayWalletRequest{
		ID: id, UserID: info.UserId, TokenID: info.TokenId, PayloadHash: hex.EncodeToString(digest[:]),
		CostBasis: &model.RelayWalletCostSnapshot{ChannelID: price.ChannelID, Model: price.Model,
			InputRate: route.UpstreamInputRate, CacheReadRate: route.UpstreamCacheReadRate, OutputRate: route.UpstreamOutputRate,
			InputLimit: price.InputLimit, OutputLimit: price.OutputLimit},
	}, price)
	switch {
	case errors.Is(err, model.ErrRelayStopped):
		return true, relayWalletReject(err, types.ErrorCode("relay_risk_stopped"), http.StatusServiceUnavailable)
	case errors.Is(err, model.ErrRelayUserBusy):
		return true, relayWalletReject(err, types.ErrorCode("relay_wallet_user_busy"), http.StatusTooManyRequests)
	case errors.Is(err, model.ErrRelayGlobalBusy):
		return true, relayWalletReject(err, types.ErrorCode("relay_wallet_global_busy"), http.StatusTooManyRequests)
	case errors.Is(err, model.ErrRelayQuota):
		return true, relayWalletReject(err, types.ErrorCodeInsufficientUserQuota, http.StatusForbidden)
	case errors.Is(err, model.ErrRelayConflict) || err == nil && !session.created:
		// Never let a replay act for, release or mark the original attempt.
		return true, relayWalletReject(errors.New("relay request id was already used"), types.ErrorCode("relay_wallet_duplicate"), http.StatusConflict)
	case err != nil:
		common.SysError(common.LogText("relay wallet reserve failed: %s", err.Error()))
		return true, relayWalletReject(errors.New("paid relay reservation failed"), types.ErrorCodeUpdateDataError, http.StatusInternalServerError)
	}
	session.stream = stream
	session.thinkingOff = route.Thinking == "disabled"
	info.Billing = session
	info.FinalPreConsumedQuota = session.reserved
	c.Header(relayWalletRequestHeader, id)
	return true, nil
}

func valueOrZero(value *uint) uint {
	if value == nil {
		return 0
	}
	return *value
}

// RejectStockRelayBilling blocks stock wallet reservations for mandatory users.
func RejectStockRelayBilling(c *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
	if c == nil || !relayWalletMandatory(c) {
		return nil
	}
	if info != nil {
		if _, durable := info.Billing.(*RelayWalletSession); durable {
			return nil
		}
	}
	return relayWalletReject(errors.New("stock billing is disabled for paid relay users"), types.ErrorCodeAccessDenied, http.StatusForbidden)
}

func (session *RelayWalletSession) price() (RelayTextPrice, error) {
	request, err := session.load()
	if err != nil {
		return RelayTextPrice{}, err
	}
	var price RelayTextPrice
	if err := common.UnmarshalJsonStr(request.PriceSnapshot, &price); err != nil {
		return RelayTextPrice{}, err
	}
	if _, err := price.rates(); err != nil {
		return RelayTextPrice{}, err
	}
	return price, nil
}

// UpstreamContext detaches the upstream call from client cancellation, so a
// stopped or disconnected chat can still read the provider's final usage.
func (session *RelayWalletSession) UpstreamContext() context.Context {
	session.CancelTransport() // A refused retry must not leak the previous timer.
	ctx, cancel := context.WithTimeout(context.Background(), relayWalletUpstreamTimeout())
	session.cancel = cancel
	return ctx
}

// CancelTransport ends the detached upstream context without a state change.
func (session *RelayWalletSession) CancelTransport() {
	if session.cancel != nil {
		session.cancel()
	}
}

// AbortDispatch is used when sending failed or was refused. Before submission
// it releases the hold; after submission the result is unknown and held.
func (session *RelayWalletSession) AbortDispatch(c *gin.Context) {
	session.CancelTransport()
	session.Refund(c)
}

// PrepareOutbound checks the final bytes after conversion, channel overrides
// and pass-through, so no setting can widen the reserved price bounds.
func (session *RelayWalletSession) PrepareOutbound(req *http.Request) error {
	if req == nil || req.Body == nil || req.URL == nil || !strings.HasSuffix(req.URL.Path, "/chat/completions") {
		return errRelayWalletOutbound
	}
	price, err := session.price()
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(req.Body, relayWalletBodyLimit+1))
	_ = req.Body.Close()
	if err != nil || len(data) > relayWalletBodyLimit {
		return errRelayWalletOutbound
	}
	var fields map[string]json.RawMessage
	if err := common.Unmarshal(data, &fields); err != nil {
		return errRelayWalletOutbound
	}
	var payload struct {
		Model         string `json:"model"`
		MaxTokens     *int   `json:"max_tokens"`
		N             *int   `json:"n"`
		Stream        *bool  `json:"stream"`
		StreamOptions *struct {
			IncludeUsage *bool `json:"include_usage"`
		} `json:"stream_options"`
		EnableThinking *bool `json:"enable_thinking"`
	}
	if err := common.Unmarshal(data, &payload); err != nil {
		return errRelayWalletOutbound
	}
	for _, key := range []string{"max_completion_tokens", "tools", "functions", "audio", "modalities", "prediction",
		"thinking", "thinking_budget", "reasoning", "reasoning_effort"} {
		if value, ok := fields[key]; ok && string(value) != "null" {
			return errRelayWalletOutbound
		}
	}
	if raw, ok := fields["enable_thinking"]; ok && (payload.EnableThinking == nil || *payload.EnableThinking) && string(raw) != "null" {
		return errRelayWalletOutbound
	}
	if session.thinkingOff && (payload.EnableThinking == nil || *payload.EnableThinking) {
		return errRelayWalletOutbound // Thinking would escape the max_tokens bound.
	}
	stream := payload.Stream != nil && *payload.Stream
	if payload.Model != price.Model || payload.MaxTokens == nil || *payload.MaxTokens <= 0 || *payload.MaxTokens > price.OutputLimit ||
		payload.N != nil && *payload.N != 1 || stream != session.stream ||
		stream && (payload.StreamOptions == nil || payload.StreamOptions.IncludeUsage == nil || !*payload.StreamOptions.IncludeUsage) {
		return errRelayWalletOutbound
	}
	req.Body = io.NopCloser(bytes.NewReader(data))
	req.ContentLength = int64(len(data))
	req.Header.Del("Content-Length")
	return nil
}

type relayWalletCancelBody struct {
	io.ReadCloser
	cancel func()
}

func (body relayWalletCancelBody) Close() error {
	err := body.ReadCloser.Close()
	body.cancel()
	return err
}

// ObserveResponse runs once after the single permitted dispatch. Explicit
// provider rejections settle at zero; any other non-200 result stays unknown.
func (session *RelayWalletSession) ObserveResponse(resp *http.Response) {
	if resp.StatusCode == http.StatusOK {
		session.observer = &relayWalletObserver{session: session, body: resp.Body, stream: session.stream}
		resp.Body = session.observer
		return
	}
	var err error
	switch resp.StatusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity, http.StatusTooManyRequests:
		err = session.settleRejected()
	default:
		err = session.Fail()
	}
	if err != nil {
		common.SysError(common.LogText("relay wallet response settlement failed (request=%s): %s", session.requestID, err.Error()))
	}
	resp.Body = relayWalletCancelBody{ReadCloser: resp.Body, cancel: session.CancelTransport}
}

func (session *RelayWalletSession) settleRejected() error {
	price, err := session.price()
	if err != nil {
		if failErr := session.Fail(); failErr != nil {
			return errors.Join(err, failErr)
		}
		return err
	}
	zero := 0
	return session.SettleTextUsage(RelayTextUsage{Model: price.Model, Complete: true, Input: &zero, CacheRead: &zero, Output: &zero, Total: &zero})
}

// Finish drains and settles an observed response. Callers skip stock
// post-consumption; the durable receipt also feeds the consume log queue.
func (session *RelayWalletSession) Finish() error {
	if session.observer == nil {
		return nil
	}
	return session.observer.Close()
}

// relayWalletObserver sees exactly the bytes the stock handler reads. It never
// rewrites them; it records the provider's original usage, model and end marker.
type relayWalletObserver struct {
	session *RelayWalletSession
	body    io.ReadCloser
	stream  bool

	mu       sync.Mutex
	buffer   []byte
	read     int
	model    string
	usage    json.RawMessage
	finished bool
	done     bool
	invalid  bool
	eof      bool

	once   sync.Once
	result error
}

func (observer *relayWalletObserver) Read(p []byte) (int, error) {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	return observer.readLocked(p)
}

func (observer *relayWalletObserver) readLocked(p []byte) (int, error) {
	if observer.eof {
		return 0, io.EOF
	}
	n, err := observer.body.Read(p)
	observer.feed(p[:n])
	if err == io.EOF {
		observer.eof = true
		observer.complete()
	} else if err != nil {
		observer.invalid = true
	}
	return n, err
}

func (observer *relayWalletObserver) feed(chunk []byte) {
	observer.read += len(chunk)
	if observer.read > relayWalletResponseLimit {
		observer.invalid = true
		return
	}
	if !observer.stream {
		observer.buffer = append(observer.buffer, chunk...)
		return
	}
	observer.buffer = append(observer.buffer, chunk...)
	for {
		index := bytes.IndexByte(observer.buffer, '\n')
		if index < 0 {
			if len(observer.buffer) > relayWalletLineLimit {
				observer.invalid = true
				observer.buffer = nil
			}
			return
		}
		line := bytes.TrimRight(observer.buffer[:index], "\r")
		observer.buffer = observer.buffer[index+1:]
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue // Comments, event names, ids and blank separators.
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if observer.done {
			observer.invalid = true // Nothing may follow the end marker.
			continue
		}
		if string(payload) == "[DONE]" {
			observer.done = true
			continue
		}
		observer.object(payload)
	}
}

func (observer *relayWalletObserver) object(payload []byte) {
	var event struct {
		Model   *string         `json:"model"`
		Usage   json.RawMessage `json:"usage"`
		Error   json.RawMessage `json:"error"`
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := common.Unmarshal(payload, &event); err != nil || len(event.Error) > 0 && string(event.Error) != "null" {
		observer.invalid = true
		return
	}
	if event.Model != nil {
		if *event.Model == "" || observer.model != "" && observer.model != *event.Model {
			observer.invalid = true
		}
		observer.model = *event.Model
	}
	if len(event.Usage) > 0 && string(event.Usage) != "null" {
		observer.usage = append(json.RawMessage(nil), event.Usage...)
	}
	for _, choice := range event.Choices {
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			observer.finished = true
		}
	}
}

func (observer *relayWalletObserver) complete() {
	if !observer.stream && !observer.invalid {
		var response struct {
			Choices []struct {
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := common.Unmarshal(observer.buffer, &response); err != nil || len(response.Choices) == 0 {
			observer.invalid = true
		} else {
			observer.object(observer.buffer)
			for _, choice := range response.Choices {
				if choice.FinishReason == nil || *choice.FinishReason == "" {
					observer.invalid = true
				}
			}
			observer.done = true
		}
		observer.buffer = nil
	}
	if observer.stream && len(bytes.TrimSpace(observer.buffer)) > 0 {
		observer.invalid = true // A truncated final event is not a completed stream.
	}
	observer.finalize()
}

func (observer *relayWalletObserver) finalize() {
	observer.once.Do(func() {
		complete := observer.eof && observer.done && observer.finished && !observer.invalid && observer.model != ""
		var usage []byte
		if complete {
			usage = observer.usage
		}
		observer.result = observer.session.SettleRawTextUsage(usage, observer.model, complete)
		observer.session.CancelTransport()
		if observer.result != nil && !errors.Is(observer.result, ErrRelayUsageUnknown) {
			common.SysError(common.LogText("relay wallet settlement failed (request=%s): %s", observer.session.requestID, observer.result.Error()))
		}
	})
}

// Close drains the remaining provider response before settling. The stock
// handler may stop reading when the client leaves; the provider may still bill.
func (observer *relayWalletObserver) Close() error {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if !observer.eof {
		buffer := make([]byte, 32<<10)
		for !observer.eof && !observer.invalid {
			if _, err := observer.readLocked(buffer); err != nil {
				break
			}
		}
	}
	observer.finalize()
	_ = observer.body.Close()
	return observer.result
}
