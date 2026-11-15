package service

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"

	"github.com/QuantumNous/new-api/model"
	"github.com/go-pay/gopay"
	"github.com/go-pay/gopay/alipay"
	"github.com/go-pay/gopay/pkg/xhttp"
)

// Per-call transport: SDK business errors omit SignData. Retain bounded raw
// bytes so BOTH success and failure are verified before touching frozen money.
// Refuse any transport-level retry/redirect; recovery uses QueryRefund only.
type relayRefundCapture struct {
	base  http.RoundTripper
	raw   []byte
	calls int
}

func (c *relayRefundCapture) RoundTrip(r *http.Request) (*http.Response, error) {
	c.calls++
	if c.calls != 1 {
		return nil, ErrRelayPayUnverified
	}
	response, err := c.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1048577))
	if err != nil || len(raw) > 1048576 || response.StatusCode != http.StatusOK {
		return nil, ErrRelayPayUnverified
	}
	c.raw = raw
	response.Body = io.NopCloser(bytes.NewReader(raw))
	return response, nil
}

func relayRefundObject(raw []byte) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, ErrRelayPayUnverified
	}
	fields := map[string]json.RawMessage{}
	for d.More() {
		token, err = d.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return nil, ErrRelayPayUnverified
		}
		if _, exists := fields[key]; exists {
			return nil, ErrRelayPayUnverified
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return nil, ErrRelayPayUnverified
		}
		fields[key] = value
	}
	if _, err := d.Token(); err != nil {
		return nil, ErrRelayPayUnverified
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, ErrRelayPayUnverified
	}
	return fields, nil
}

func refundBound(order model.RelayPayOrder, refund model.RelayRefund, channel RelayPayChannel) bool {
	return order.ID == refund.OrderID && order.UserID == refund.UserID && order.Channel == channel.Name() &&
		order.MerchantApp == channel.App() && order.ConfigVersion == channel.Version() && order.TradeNo != nil &&
		refund.AmountFen > 0 && refund.AmountFen <= order.AmountFen && refund.Quota == refund.AmountFen*10000 && refund.ID != ""
}

func refundFact(order model.RelayPayOrder, refund model.RelayRefund, state, evidence string) model.RelayRefundFact {
	return model.RelayRefundFact{RefundID: refund.ID, OrderID: order.ID, Channel: order.Channel, App: order.MerchantApp,
		TradeNo: *order.TradeNo, AmountFen: refund.AmountFen, Status: state, Evidence: relayPayDigest(evidence)}
}

func (a *relayPayAlipay) Refund(ctx context.Context, order model.RelayPayOrder, refund model.RelayRefund) (model.RelayRefundFact, error) {
	return a.refundCall(ctx, order, refund, false)
}
func (a *relayPayAlipay) QueryRefund(ctx context.Context, order model.RelayPayOrder, refund model.RelayRefund) (model.RelayRefundFact, error) {
	return a.refundCall(ctx, order, refund, true)
}

func (a *relayPayAlipay) refundCall(ctx context.Context, order model.RelayPayOrder, refund model.RelayRefund, query bool) (model.RelayRefundFact, error) {
	if !refundBound(order, refund, a) {
		return model.RelayRefundFact{}, model.ErrRelayPayMismatch
	}
	base := a.transport
	if base == nil {
		base = http.DefaultTransport
	}
	capture := &relayRefundCapture{base: base}
	client, err := alipay.NewClient(a.config.AppID, a.config.PrivateKey, a.config.Production)
	if err != nil {
		return model.RelayRefundFact{}, err
	}
	hc := xhttp.NewClient()
	hc.SetTransport(capture)
	client.SetHttpClient(hc)
	bm := make(gopay.BodyMap)
	bm.Set("out_trade_no", order.ID).Set("trade_no", *order.TradeNo).Set("out_request_no", refund.ID)
	method := "alipay_trade_refund_response"
	if query {
		method = "alipay_trade_fastpay_refund_query_response"
		_, err = client.TradeFastPayRefundQuery(ctx, bm)
	} else {
		bm.Set("refund_amount", relayPayAmount(refund.AmountFen))
		_, err = client.TradeRefund(ctx, bm)
	}
	if len(capture.raw) == 0 {
		return model.RelayRefundFact{}, errors.New("refund channel unavailable; result unknown")
	}
	// Never trust SDK-decoded fields or an SDK error until raw RSA2 verifies.
	envelope, verifyErr := relayRefundObject(capture.raw)
	if verifyErr != nil {
		return model.RelayRefundFact{}, verifyErr
	}
	var signature string
	if json.Unmarshal(envelope["sign"], &signature) != nil {
		return model.RelayRefundFact{}, ErrRelayPayUnverified
	}
	sig, verifyErr := base64.StdEncoding.DecodeString(signature)
	payload := envelope[method]
	digest := sha256.Sum256(payload)
	if verifyErr != nil || len(payload) == 0 || rsa.VerifyPKCS1v15(a.public, crypto.SHA256, digest[:], sig) != nil {
		return model.RelayRefundFact{}, ErrRelayPayUnverified
	}
	if _, verifyErr = relayRefundObject(payload); verifyErr != nil {
		return model.RelayRefundFact{}, verifyErr
	}
	var result struct {
		Code       string `json:"code"`
		SubCode    string `json:"sub_code"`
		TradeNo    string `json:"trade_no"`
		OrderID    string `json:"out_trade_no"`
		RefundID   string `json:"out_request_no"`
		Fee        string `json:"refund_fee"`
		Amount     string `json:"refund_amount"`
		Currency   string `json:"refund_currency"`
		Status     string `json:"refund_status"`
		FundChange string `json:"fund_change"`
	}
	if json.Unmarshal(payload, &result) != nil {
		return model.RelayRefundFact{}, ErrRelayPayUnverified
	}
	state := "unknown"
	if result.OrderID != "" && result.OrderID != order.ID || result.TradeNo != "" && result.TradeNo != *order.TradeNo || result.RefundID != "" && result.RefundID != refund.ID || result.Currency != "" && result.Currency != "CNY" {
		return model.RelayRefundFact{}, ErrRelayPayUnverified
	}
	if result.Code == "10000" {
		if err != nil || result.OrderID != order.ID || result.TradeNo != *order.TradeNo ||
			result.Currency != "" && result.Currency != "CNY" || result.RefundID != "" && result.RefundID != refund.ID {
			return model.RelayRefundFact{}, ErrRelayPayUnverified
		}
		amount := result.Fee
		if query {
			amount = result.Amount
		}
		fen, ok := relayPayFen(amount)
		if !ok || fen != refund.AmountFen {
			return model.RelayRefundFact{}, ErrRelayPayUnverified
		}
		if query && result.RefundID == refund.ID && result.Status == "REFUND_SUCCESS" {
			state = "succeeded"
		}
		// Direct responses do not reliably echo out_request_no. Confirm by
		// query rather than accepting a replayed receipt from a different refund.
	} else if !query && result.Code == "40004" {
		switch result.SubCode {
		case "ACQ.REFUND_AMT_NOT_EQUAL_TOTAL", "ACQ.TRADE_NOT_ALLOW_REFUND":
			state = "failed_released"
		}
	}
	return refundFact(order, refund, state, string(payload)), nil
}

// Mock outcomes are local configuration, never browser inputs. Default stays
// unknown; tests opt into success/failure/timeout explicitly.
func (m relayPayMock) Refund(ctx context.Context, order model.RelayPayOrder, refund model.RelayRefund) (model.RelayRefundFact, error) {
	return m.refundResult(ctx, order, refund, false)
}
func (m relayPayMock) QueryRefund(ctx context.Context, order model.RelayPayOrder, refund model.RelayRefund) (model.RelayRefundFact, error) {
	return m.refundResult(ctx, order, refund, true)
}
func (m relayPayMock) refundResult(ctx context.Context, order model.RelayPayOrder, refund model.RelayRefund, query bool) (model.RelayRefundFact, error) {
	if !refundBound(order, refund, m) {
		return model.RelayRefundFact{}, model.ErrRelayPayMismatch
	}
	if err := ctx.Err(); err != nil {
		return model.RelayRefundFact{}, err
	}
	mode := os.Getenv("RELAY_REFUND_MOCK_RESULT")
	if query {
		mode = os.Getenv("RELAY_REFUND_MOCK_QUERY_RESULT")
	}
	if mode == "timeout" {
		return model.RelayRefundFact{}, context.DeadlineExceeded
	}
	state := "unknown"
	if mode == "success" {
		state = "succeeded"
	}
	if mode == "failure" && !query {
		state = "failed_released"
	}
	return refundFact(order, refund, state, "mock-refund:"+refund.ID+":"+state), nil
}
