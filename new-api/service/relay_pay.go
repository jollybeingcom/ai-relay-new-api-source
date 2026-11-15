package service

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/go-pay/gopay"
	"github.com/go-pay/gopay/alipay"
	"github.com/go-pay/gopay/pkg/xhttp"
)

// Confirmed top-up tiers in CNY. The browser chooses a tier, never an amount.
var RelayPayTiers = []int64{10, 30, 50, 100}

const (
	RelayPayRateVersion  = "cny-1e6-v1" // 1 CNY = RelayQuotaPerCNY quota.
	relayPayOrderMinutes = 15
	relayPayMaxPending   = 3
)

var (
	ErrRelayPayDisabled   = errors.New("top-up is not available")
	ErrRelayPayUnverified = errors.New("payment notification failed verification")
	relayPayAmountPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,8})\.([0-9]{2})$`)
)

// RelayPayChannel adapts one provider. Create returns where to send the payer;
// Verify and Query return only facts whose signature has been checked.
type RelayPayChannel interface {
	Name() string
	App() string
	Version() string
	Create(ctx context.Context, order model.RelayPayOrder, mobile bool) (string, error)
	Verify(req *http.Request) (model.RelayPayFact, error)
	Query(ctx context.Context, order model.RelayPayOrder) (model.RelayPayFact, error)
	Refund(ctx context.Context, order model.RelayPayOrder, refund model.RelayRefund) (model.RelayRefundFact, error)
	QueryRefund(ctx context.Context, order model.RelayPayOrder, refund model.RelayRefund) (model.RelayRefundFact, error)
}

type relayPayConfig struct {
	AppID           string `json:"app_id"`
	PrivateKey      string `json:"private_key"`
	AlipayPublicKey string `json:"alipay_public_key"`
	SellerID        string `json:"seller_id"`
	NotifyURL       string `json:"notify_url"`
	ReturnURL       string `json:"return_url"`
	Production      bool   `json:"production"`
	ConfigVersion   string `json:"config_version"`
}

type relayPayLoaded struct {
	channel RelayPayChannel
	err     error
}

var relayPayChannels = struct {
	sync.Mutex
	byConfig map[string]relayPayLoaded
}{byConfig: map[string]relayPayLoaded{}}

// CurrentRelayPayChannel loads each configuration once per process; merchant
// key changes need a new config_version and a restart. RELAY_PAY_CHANNEL is
// "alipay" (with RELAY_PAY_ALIPAY_FILE) or "mock" for isolated development only.
func CurrentRelayPayChannel() (RelayPayChannel, error) {
	key := os.Getenv("RELAY_PAY_CHANNEL") + "\x00" + os.Getenv("RELAY_PAY_ALIPAY_FILE")
	relayPayChannels.Lock()
	defer relayPayChannels.Unlock()
	loaded, ok := relayPayChannels.byConfig[key]
	if !ok {
		loaded.channel, loaded.err = loadRelayPayChannel()
		relayPayChannels.byConfig[key] = loaded
	}
	return loaded.channel, loaded.err
}

func loadRelayPayChannel() (RelayPayChannel, error) {
	switch os.Getenv("RELAY_PAY_CHANNEL") {
	case "":
		return nil, ErrRelayPayDisabled
	case "mock":
		return relayPayMock{}, nil
	case "alipay":
		data, err := os.ReadFile(os.Getenv("RELAY_PAY_ALIPAY_FILE"))
		if err != nil {
			return nil, err
		}
		var config relayPayConfig
		if err := common.Unmarshal(data, &config); err != nil {
			return nil, err
		}
		return NewRelayPayAlipay(config, nil)
	}
	return nil, ErrRelayPayDisabled
}

// NewRelayPayOrder builds the immutable snapshot from a tier and the channel.
func NewRelayPayOrder(userID int, yuan int64, channel RelayPayChannel) (model.RelayPayOrder, error) {
	valid := false
	for _, tier := range RelayPayTiers {
		valid = valid || tier == yuan
	}
	if !valid || channel == nil {
		return model.RelayPayOrder{}, model.ErrRelayInput
	}
	raw := make([]byte, 15)
	if _, err := rand.Read(raw); err != nil {
		return model.RelayPayOrder{}, err
	}
	return model.CreateRelayPayOrder(model.DB, model.RelayPayOrder{ID: "RP" + hex.EncodeToString(raw), UserID: userID,
		AmountFen: yuan * 100, Quota: yuan * RelayQuotaPerCNY, Currency: "CNY", Channel: channel.Name(), MerchantApp: channel.App(),
		ConfigVersion: channel.Version(), RateVersion: RelayPayRateVersion,
		ExpiresAt: time.Now().Add(relayPayOrderMinutes * time.Minute).Unix()}, relayPayMaxPending)
}

func relayPayFen(amount string) (int64, bool) {
	match := relayPayAmountPattern.FindStringSubmatch(amount)
	if match == nil {
		return 0, false
	}
	var yuan, fen int64
	fmt.Sscan(match[1], &yuan)
	fmt.Sscan(match[2], &fen)
	return yuan*100 + fen, true
}

func relayPayAmount(fen int64) string { return fmt.Sprintf("%d.%02d", fen/100, fen%100) }

func relayPayDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// relayPayAlipay uses GoPay for request signing, notification verification and
// trade queries. Public-key mode; RSA2 (SHA256withRSA) only.
type relayPayAlipay struct {
	config    relayPayConfig
	client    *alipay.Client
	public    *rsa.PublicKey
	transport http.RoundTripper
}

func NewRelayPayAlipay(config relayPayConfig, transport http.RoundTripper) (RelayPayChannel, error) {
	if !regexp.MustCompile(`^[0-9]{8,32}$`).MatchString(config.AppID) || config.ConfigVersion == "" || len(config.ConfigVersion) > 64 ||
		!strings.HasPrefix(config.NotifyURL, "https://") || config.ReturnURL != "" && !strings.HasPrefix(config.ReturnURL, "https://") {
		return nil, model.ErrRelayInput
	}
	client, err := alipay.NewClient(config.AppID, config.PrivateKey, config.Production)
	if err != nil {
		return nil, err
	}
	client.SetNotifyUrl(config.NotifyURL)
	if config.ReturnURL != "" {
		client.SetReturnUrl(config.ReturnURL)
	}
	if transport != nil {
		hc := xhttp.NewClient()
		hc.SetTransport(transport)
		client.SetHttpClient(hc)
	}
	public, err := relayPayPublicKey(config.AlipayPublicKey)
	if err != nil {
		return nil, err
	}
	return &relayPayAlipay{config: config, client: client, public: public, transport: transport}, nil
}

func relayPayPublicKey(raw string) (*rsa.PublicKey, error) {
	text := strings.TrimSpace(raw)
	if !strings.Contains(text, "BEGIN") {
		text = "-----BEGIN PUBLIC KEY-----\n" + text + "\n-----END PUBLIC KEY-----"
	}
	block, _ := pem.Decode([]byte(text))
	if block == nil {
		return nil, model.ErrRelayInput
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	public, ok := key.(*rsa.PublicKey)
	if !ok || public.N.BitLen() < 2048 {
		return nil, model.ErrRelayInput
	}
	return public, nil
}

func (a *relayPayAlipay) Name() string    { return "alipay" }
func (a *relayPayAlipay) App() string     { return a.config.AppID }
func (a *relayPayAlipay) Version() string { return a.config.ConfigVersion }

func (a *relayPayAlipay) Create(ctx context.Context, order model.RelayPayOrder, mobile bool) (string, error) {
	bm := make(gopay.BodyMap)
	bm.Set("out_trade_no", order.ID).Set("total_amount", relayPayAmount(order.AmountFen)).
		Set("subject", fmt.Sprintf("AI 中转站余额充值 %d 元", order.AmountFen/100)).
		Set("time_expire", time.Unix(order.ExpiresAt, 0).In(time.FixedZone("CST", 8*3600)).Format("2006-01-02 15:04:05"))
	if mobile {
		return a.client.TradeWapPay(ctx, bm)
	}
	return a.client.TradePagePay(ctx, bm)
}

func relayPayStatus(status string) (string, bool) {
	switch status {
	case "TRADE_SUCCESS", "TRADE_FINISHED":
		return "paid", true
	case "TRADE_CLOSED":
		return "closed", true
	case "WAIT_BUYER_PAY":
		return "pending", true
	}
	return "", false
}

// Verify checks the RSA2 signature first, then the merchant and amount fields.
// GoPay falls back to SHA256 for an unknown sign_type and accepts SHA1 "RSA";
// both are refused here before verification.
func (a *relayPayAlipay) Verify(req *http.Request) (model.RelayPayFact, error) {
	if err := req.ParseForm(); err != nil {
		return model.RelayPayFact{}, ErrRelayPayUnverified
	}
	for _, values := range req.PostForm {
		if len(values) != 1 {
			return model.RelayPayFact{}, ErrRelayPayUnverified // Ambiguous duplicate fields.
		}
	}
	bm, err := alipay.ParseNotifyByURLValues(req.PostForm)
	if err != nil || bm.GetString("sign_type") != "RSA2" {
		return model.RelayPayFact{}, ErrRelayPayUnverified
	}
	canonical := req.PostForm.Encode()
	ok, err := alipay.VerifySign(a.config.AlipayPublicKey, bm)
	if err != nil || !ok {
		return model.RelayPayFact{}, ErrRelayPayUnverified
	}
	status, known := relayPayStatus(bm.GetString("trade_status"))
	fen, valid := relayPayFen(bm.GetString("total_amount"))
	if !known || !valid || a.config.SellerID != "" && bm.GetString("seller_id") != a.config.SellerID {
		return model.RelayPayFact{}, ErrRelayPayUnverified
	}
	return model.RelayPayFact{OrderID: bm.GetString("out_trade_no"), Channel: "alipay", App: bm.GetString("app_id"),
		TradeNo: bm.GetString("trade_no"), AmountFen: fen, Currency: "CNY", Status: status, Evidence: relayPayDigest(canonical)}, nil
}

// Query verifies the synchronous response signature as well; GoPay only does
// so automatically in certificate mode.
func (a *relayPayAlipay) Query(ctx context.Context, order model.RelayPayOrder) (model.RelayPayFact, error) {
	bm := make(gopay.BodyMap)
	bm.Set("out_trade_no", order.ID)
	response, err := a.client.TradeQuery(ctx, bm)
	if response != nil && response.Response != nil && response.Response.SubCode == "ACQ.TRADE_NOT_EXIST" {
		if time.Now().Unix() > order.ExpiresAt+600 {
			return model.RelayPayFact{OrderID: order.ID, Channel: "alipay", App: a.config.AppID, Status: "closed",
				Evidence: relayPayDigest("not_exist:" + order.ID)}, nil
		}
		return model.RelayPayFact{OrderID: order.ID, Channel: "alipay", App: a.config.AppID, Status: "pending",
			Evidence: relayPayDigest("not_exist:" + order.ID)}, nil
	}
	if err != nil {
		return model.RelayPayFact{}, err // Network or provider errors stay pending, never failed.
	}
	signature, err := base64.StdEncoding.DecodeString(response.Sign)
	digest := sha256.Sum256([]byte(response.SignData))
	if err != nil || response.SignData == "" || rsa.VerifyPKCS1v15(a.public, crypto.SHA256, digest[:], signature) != nil {
		return model.RelayPayFact{}, ErrRelayPayUnverified
	}
	trade := response.Response
	status, known := relayPayStatus(trade.TradeStatus)
	fen, valid := relayPayFen(trade.TotalAmount)
	if !known || !valid || trade.OutTradeNo != order.ID || trade.TransCurrency != "" && trade.TransCurrency != "CNY" {
		return model.RelayPayFact{}, ErrRelayPayUnverified
	}
	return model.RelayPayFact{OrderID: order.ID, Channel: "alipay", App: a.config.AppID, TradeNo: trade.TradeNo,
		AmountFen: fen, Currency: "CNY", Status: status, Evidence: relayPayDigest(response.SignData)}, nil
}

// relayPayMock is an isolated development channel. It has no notification
// endpoint; only an administrator action can produce its success fact.
type relayPayMock struct{}

func (relayPayMock) Name() string    { return "mock" }
func (relayPayMock) App() string     { return "mock-local" }
func (relayPayMock) Version() string { return "mock-v1" }
func (relayPayMock) Create(_ context.Context, order model.RelayPayOrder, _ bool) (string, error) {
	return "/relay-pay-mock?order=" + url.QueryEscape(order.ID), nil
}
func (relayPayMock) Verify(*http.Request) (model.RelayPayFact, error) {
	return model.RelayPayFact{}, ErrRelayPayUnverified
}
func (relayPayMock) Query(_ context.Context, order model.RelayPayOrder) (model.RelayPayFact, error) {
	return model.RelayPayFact{OrderID: order.ID, Channel: "mock", App: "mock-local", Status: "pending", Evidence: relayPayDigest("mock:" + order.ID)}, nil
}

// MockRelayPaySuccess simulates a verified provider success for the mock channel.
func MockRelayPaySuccess(order model.RelayPayOrder) (model.RelayPayOrder, error) {
	if order.Channel != "mock" {
		return model.RelayPayOrder{}, ErrRelayPayDisabled
	}
	return model.ApplyRelayPayFact(model.DB, model.RelayPayFact{OrderID: order.ID, Channel: "mock", App: "mock-local",
		TradeNo: "MOCK" + order.ID[2:], AmountFen: order.AmountFen, Currency: "CNY", Status: "paid", Evidence: relayPayDigest("mock-paid:" + order.ID)})
}

// SyncRelayPayOrders is the bounded background query pass.
func SyncRelayPayOrders(ctx context.Context, limit int) (int, error) {
	channel, err := CurrentRelayPayChannel()
	if err != nil {
		return 0, nil // Disabled channels still accept no new orders; nothing to query.
	}
	orders, err := model.DueRelayPayOrders(model.DB, limit)
	if err != nil {
		return 0, err
	}
	changed := 0
	var failures []error
	for _, order := range orders {
		if order.Channel != channel.Name() || order.MerchantApp != channel.App() {
			failures = append(failures, model.ScheduleRelayPayQuery(model.DB, order))
			continue // Orders from another merchant config need manual review.
		}
		queryCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		fact, err := channel.Query(queryCtx, order)
		cancel()
		if err == nil {
			var applied model.RelayPayOrder
			applied, err = model.ApplyRelayPayFact(model.DB, fact)
			if err == nil && applied.State != "pending" {
				changed++
				continue
			}
		}
		if err != nil {
			common.SysError(common.LogText("relay pay query failed (order=%s): %s", order.ID, err.Error()))
		}
		failures = append(failures, model.ScheduleRelayPayQuery(model.DB, order))
	}
	return changed, errors.Join(failures...)
}

// RelayPayOrderView is the owner-facing projection; no merchant or trade data.
type RelayPayOrderView struct {
	ID        string `json:"id"`
	Amount    string `json:"amount_cny"`
	State     string `json:"state"`
	Credited  bool   `json:"credited"`
	Review    bool   `json:"needs_review"`
	CreatedAt int64  `json:"created_at"`
	ExpiresAt int64  `json:"expires_at"`
	PaidAt    int64  `json:"paid_at"`
}

func ViewRelayPayOrder(order model.RelayPayOrder) RelayPayOrderView {
	return RelayPayOrderView{ID: order.ID, Amount: relayPayAmount(order.AmountFen), State: order.State, Credited: order.Credit == "credited",
		Review: order.Credit == "attention", CreatedAt: order.CreatedAt, ExpiresAt: order.ExpiresAt, PaidAt: order.PaidAt}
}
