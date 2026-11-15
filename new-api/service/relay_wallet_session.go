package service

import (
	"context"
	"errors"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

var ErrRelayAlreadyDispatched = errors.New("relay request no longer permits upstream dispatch")

// A fresh HTTP/1 transport prevents hidden HTTP/2 or reused-connection retries.
// Keep the upstream proxy/TLS policy, without mutating the shared client cache.
func NewRelayWalletHTTPClient(settings dto.ChannelSettings) (*http.Client, error) {
	settings.HTTPProtocol = dto.HTTPProtocolHTTP1
	settings.HTTP2ConnectionShards = 1
	client, err := GetHttpClientWithProxySettings(settings.Proxy, settings)
	if err != nil {
		return nil, err
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		return nil, errors.New("relay wallet requires a standard HTTP/1 transport")
	}
	isolated := *client
	cloned := transport.Clone()
	cloned.DisableKeepAlives = true
	isolated.Transport = cloned
	return &isolated, nil
}

// RelayWalletSession adapts the durable ledger to the upstream billing contract.
// Live chat selects it only behind RELAY_WALLET_BILLING (see relay_wallet_chat.go).
// The intent must come from trusted server-side data.
type RelayWalletSession struct {
	db        *gorm.DB
	requestID string
	userID    int
	reserved  int
	created   bool // false when another attempt already owns this business key
	stream    bool
	// thinkingOff requires the outbound body to disable reasoning explicitly.
	thinkingOff bool
	observer    *relayWalletObserver
	cancel      context.CancelFunc
}

var _ relaycommon.BillingSettler = (*RelayWalletSession)(nil)

func NewRelayWalletSession(db *gorm.DB, intent model.RelayWalletRequest) (*RelayWalletSession, error) {
	if db == nil {
		return nil, model.ErrRelayInput
	}
	limits, err := ActiveRelayWalletLimits()
	if err != nil {
		return nil, err
	}
	request, created, err := model.ReserveRelayWalletCreated(db, intent, limits)
	if err != nil {
		return nil, err
	}
	return &RelayWalletSession{db: db, requestID: request.ID, userID: request.UserID, reserved: int(request.Reserved), created: created}, nil
}

func (session *RelayWalletSession) GetPreConsumedQuota() int { return session.reserved }

func (session *RelayWalletSession) Reserve(targetQuota int) error {
	return session.ReserveBeforeSubmit(targetQuota)
}

func (session *RelayWalletSession) ReserveBeforeSubmit(targetQuota int) error {
	if targetQuota < 0 || targetQuota > session.reserved {
		return model.ErrRelayInput
	}
	request, err := session.load()
	if err != nil {
		return err
	}
	if request.State != "reserved" {
		return ErrRelayAlreadyDispatched
	}
	return nil
}

// Submit must succeed immediately before sending. Replays return an error,
// rather than a successful no-op that a transport might mistake for permission.
func (session *RelayWalletSession) Submit() error {
	changed, err := model.AdvanceRelayWallet(session.db, session.requestID, session.userID, "submitted", 0)
	if err != nil {
		return err
	}
	if !changed {
		return ErrRelayAlreadyDispatched
	}
	return nil
}

func (session *RelayWalletSession) Settle(actualQuota int) error {
	request, err := session.load()
	if err != nil {
		return err
	}
	var snapshot struct {
		Schema string `json:"schema"`
	}
	if err := common.UnmarshalJsonStr(request.PriceSnapshot, &snapshot); err != nil {
		return err
	}
	// A priced request must carry normalized facts through SettleTextUsage.
	// The stock integer-only BillingSettler path cannot bypass its receipt.
	if snapshot.Schema == "text-v1" {
		return ErrRelayUsageUnknown
	}
	_, err = model.AdvanceRelayWallet(session.db, session.requestID, session.userID, "settled", int64(actualQuota))
	return err
}

func (session *RelayWalletSession) SubmitForChannel(channelID int) error {
	request, err := session.load()
	if err != nil {
		return err
	}
	var snapshot struct {
		Schema    string `json:"schema"`
		ChannelID int    `json:"channel_id"`
	}
	if err := common.UnmarshalJsonStr(request.PriceSnapshot, &snapshot); err != nil {
		return err
	}
	if snapshot.Schema == "text-v1" && (channelID <= 0 || channelID != snapshot.ChannelID) {
		return model.ErrRelayConflict
	}
	return session.Submit()
}

func (session *RelayWalletSession) load() (model.RelayWalletRequest, error) {
	var request model.RelayWalletRequest
	err := session.db.Where("id = ? AND user_id = ?", session.requestID, session.userID).Take(&request).Error
	return request, err
}

func (session *RelayWalletSession) NeedsRefund() bool {
	request, err := session.load()
	// Preserve a cleanup attempt when the database is temporarily unavailable.
	return err != nil || request.State == "reserved" || request.State == "submitted"
}

// Fail is synchronous and returns persistence failures to direct callers.
// A concurrent dispatch/settlement may advance the state between load and CAS;
// re-reading once per possible forward state does not retry any upstream call.
func (session *RelayWalletSession) Fail() error {
	for range 3 {
		request, err := session.load()
		if err != nil {
			return err
		}
		target := ""
		switch request.State {
		case "reserved":
			target = "released"
		case "submitted":
			target = "unknown"
		case "unknown", "settled", "released":
			return nil
		default:
			return model.ErrRelayState
		}
		_, err = model.AdvanceRelayWallet(session.db, session.requestID, session.userID, target, 0)
		if errors.Is(err, model.ErrRelayState) {
			continue
		}
		return err
	}
	return model.ErrRelayState
}

// The stock interface cannot return an error; persist first, then log failures.
// Recovery still sees the original durable state after a failed transaction.
func (session *RelayWalletSession) Refund(_ *gin.Context) {
	if err := session.Fail(); err != nil {
		common.SysError(common.LogText("relay wallet cleanup failed (request=%s): %s", session.requestID, err.Error()))
	}
}
