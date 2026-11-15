package service

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Isolated test keys only. Signing follows Alipay's published RSA2 rule and is
// implemented here independently of GoPay, so verification is not self-confirming.
type relayPayKeys struct {
	app, alipay *rsa.PrivateKey
}

func newRelayPayKeys(t *testing.T) relayPayKeys {
	t.Helper()
	app, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	ali, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	return relayPayKeys{app: app, alipay: ali}
}

func relayPayPublicBase64(t *testing.T, key *rsa.PrivateKey) string {
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(der)
}

func relayPaySign(t *testing.T, key *rsa.PrivateKey, content string) string {
	digest := sha256.Sum256([]byte(content))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(signature)
}

// Alipay async rule: drop sign and sign_type, drop empty values, sort keys,
// join raw "k=v" pairs with "&".
func relayPayCanonical(values url.Values) string { return relayPayCanonicalForm(values, false) }

// Request signing keeps sign_type; only async notifications drop it.
func relayPayCanonicalForm(values url.Values, keepSignType bool) string {
	keys := make([]string, 0, len(values))
	for key, value := range values {
		if key != "sign" && (keepSignType || key != "sign_type") && value[0] != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+values.Get(key))
	}
	return strings.Join(parts, "&")
}

func relayPayAlipayFixture(t *testing.T, keys relayPayKeys, transport http.RoundTripper) RelayPayChannel {
	t.Helper()
	appDER := x509.MarshalPKCS1PrivateKey(keys.app)
	channel, err := NewRelayPayAlipay(relayPayConfig{AppID: "2021000000000001", PrivateKey: base64.StdEncoding.EncodeToString(appDER),
		AlipayPublicKey: relayPayPublicBase64(t, keys.alipay), SellerID: "2088000000000001", NotifyURL: "https://pay.example.test/api/relay_pay/notify/alipay",
		ReturnURL: "https://pay.example.test/topup", ConfigVersion: "fixture-v1"}, transport)
	require.NoError(t, err)
	return channel
}

func relayPayNotify(t *testing.T, keys relayPayKeys, change func(url.Values)) *http.Request {
	values := url.Values{"app_id": {"2021000000000001"}, "out_trade_no": {"RP" + strings.Repeat("a", 30)}, "trade_no": {"2026102722001400001"},
		"total_amount": {"30.00"}, "trade_status": {"TRADE_SUCCESS"}, "seller_id": {"2088000000000001"}, "notify_id": {"n1"},
		"notify_time": {"2026-10-27 15:00:00"}, "subject": {"AI 中转站余额充值 30 元"}, "charset": {"utf-8"}, "version": {"1.0"},
		"sign_type": {"RSA2"}}
	values.Set("sign", relayPaySign(t, keys.alipay, relayPayCanonical(values)))
	if change != nil {
		change(values)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/relay_pay/notify/alipay", strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return request
}

func TestRelayPayAlipayNotification(t *testing.T) {
	keys := newRelayPayKeys(t)
	channel := relayPayAlipayFixture(t, keys, nil)
	fact, err := channel.Verify(relayPayNotify(t, keys, nil))
	require.NoError(t, err)
	assert.Equal(t, model.RelayPayFact{OrderID: "RP" + strings.Repeat("a", 30), Channel: "alipay", App: "2021000000000001",
		TradeNo: "2026102722001400001", AmountFen: 3000, Currency: "CNY", Status: "paid", Evidence: fact.Evidence}, fact)
	assert.Len(t, fact.Evidence, 64)

	other := newRelayPayKeys(t)
	resign := func(values url.Values) {
		values.Set("sign", relayPaySign(t, keys.alipay, relayPayCanonical(values)))
	}
	cases := map[string]func(url.Values){
		"tampered_amount": func(v url.Values) { v.Set("total_amount", "3000.00") },
		"tampered_status": func(v url.Values) {
			v.Set("trade_status", "TRADE_SUCCESS ")
			v.Set("out_trade_no", "RP"+strings.Repeat("b", 30))
		},
		"wrong_signing_key":       func(v url.Values) { v.Set("sign", relayPaySign(t, other.alipay, relayPayCanonical(v))) },
		"sha1_downgrade":          func(v url.Values) { v.Set("sign_type", "RSA") },
		"missing_sign_type":       func(v url.Values) { v.Del("sign_type") },
		"garbage_signature":       func(v url.Values) { v.Set("sign", "bm90LWEtc2lnbmF0dXJl") },
		"other_seller":            func(v url.Values) { v.Set("seller_id", "2088999999999999"); resign(v) },
		"unknown_status":          func(v url.Values) { v.Set("trade_status", "TRADE_REFUNDED"); resign(v) },
		"fractional_fen":          func(v url.Values) { v.Set("total_amount", "30.001"); resign(v) },
		"duplicate_field":         func(v url.Values) { v.Add("total_amount", "30.00") },
		"signed_by_merchant_key":  func(v url.Values) { v.Set("sign", relayPaySign(t, keys.app, relayPayCanonical(v))) },
		"browser_return_has_none": func(v url.Values) { v.Del("sign") },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := channel.Verify(relayPayNotify(t, keys, change))
			assert.ErrorIs(t, err, ErrRelayPayUnverified)
		})
	}
	closed, err := channel.Verify(relayPayNotify(t, keys, func(v url.Values) { v.Set("trade_status", "TRADE_CLOSED"); resign(v) }))
	require.NoError(t, err)
	assert.Equal(t, "closed", closed.Status)
}

type relayPayRoundTrip func(*http.Request) (*http.Response, error)

func (f relayPayRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRelayPayAlipayCreateAndQuery(t *testing.T) {
	keys := newRelayPayKeys(t)
	order := model.RelayPayOrder{ID: "RP" + strings.Repeat("c", 30), AmountFen: 1000, ExpiresAt: time.Now().Add(15 * time.Minute).Unix()}
	var body string
	var respond func(*http.Request) string
	transport := relayPayRoundTrip(func(r *http.Request) (*http.Response, error) {
		assert.Equal(t, "openapi.alipay.com", r.URL.Host, "production gateway, intercepted by the isolated transport")
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(respond(r)))}, nil
	})
	channel := relayPayAlipayFixture(t, keys, transport)
	channel.(*relayPayAlipay).client.IsProd = true

	t.Run("page_and_wap_urls_are_signed_by_the_merchant_key", func(t *testing.T) {
		for _, mobile := range []bool{false, true} {
			payURL, err := channel.Create(context.Background(), order, mobile)
			require.NoError(t, err)
			parsed, err := url.Parse(payURL)
			require.NoError(t, err)
			query := parsed.Query()
			assert.Equal(t, map[bool]string{false: "alipay.trade.page.pay", true: "alipay.trade.wap.pay"}[mobile], query.Get("method"))
			assert.Equal(t, "https://pay.example.test/api/relay_pay/notify/alipay", query.Get("notify_url"))
			var biz map[string]any
			require.NoError(t, json.Unmarshal([]byte(query.Get("biz_content")), &biz))
			assert.Equal(t, order.ID, biz["out_trade_no"])
			assert.Equal(t, "10.00", biz["total_amount"])
			signature, err := base64.StdEncoding.DecodeString(query.Get("sign"))
			require.NoError(t, err)
			digest := sha256.Sum256([]byte(relayPayCanonicalForm(query, true)))
			assert.NoError(t, rsa.VerifyPKCS1v15(&keys.app.PublicKey, crypto.SHA256, digest[:], signature))
		}
	})

	signed := func(response string, key *rsa.PrivateKey) string {
		return `{"alipay_trade_query_response":` + response + `,"sign":"` + relaySignJSON(t, key, response) + `"}`
	}
	success := `{"code":"10000","msg":"Success","trade_no":"2026102722001400002","out_trade_no":"` + order.ID + `","trade_status":"TRADE_SUCCESS","total_amount":"10.00"}`
	cases := []struct {
		name, response string
		status         string
		err            error
	}{
		{"paid", signed(success, keys.alipay), "paid", nil},
		{"waiting", signed(strings.Replace(success, "TRADE_SUCCESS", "WAIT_BUYER_PAY", 1), keys.alipay), "pending", nil},
		{"closed", signed(strings.Replace(success, "TRADE_SUCCESS", "TRADE_CLOSED", 1), keys.alipay), "closed", nil},
		{"forged_signature", signed(success, newRelayPayKeys(t).alipay), "", ErrRelayPayUnverified},
		{"amount_edited_after_signing", strings.Replace(signed(success, keys.alipay), `"10.00"`, `"100.00"`, 1), "", ErrRelayPayUnverified},
		{"other_order", signed(strings.Replace(success, order.ID, "RP"+strings.Repeat("d", 30), 1), keys.alipay), "", ErrRelayPayUnverified},
		{"not_yet_created", signed(`{"code":"40004","msg":"Business Failed","sub_code":"ACQ.TRADE_NOT_EXIST","sub_msg":"x"}`, keys.alipay), "pending", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			respond = func(*http.Request) string { return tc.response }
			fact, err := channel.Query(context.Background(), order)
			values, parseErr := url.ParseQuery(body)
			require.NoError(t, parseErr)
			assert.Equal(t, "alipay.trade.query", values.Get("method"))
			if tc.err != nil {
				assert.ErrorIs(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.status, fact.Status)
			if tc.status == "paid" {
				assert.Equal(t, int64(1000), fact.AmountFen)
				assert.Equal(t, "2026102722001400002", fact.TradeNo)
			}
		})
	}
	t.Run("expired_missing_trade_is_closed", func(t *testing.T) {
		respond = func(*http.Request) string {
			return signed(`{"code":"40004","msg":"Business Failed","sub_code":"ACQ.TRADE_NOT_EXIST","sub_msg":"x"}`, keys.alipay)
		}
		expired := order
		expired.ExpiresAt = time.Now().Add(-time.Hour).Unix()
		fact, err := channel.Query(context.Background(), expired)
		require.NoError(t, err)
		assert.Equal(t, "closed", fact.Status)
	})
	t.Run("transport_failure_stays_unknown", func(t *testing.T) {
		failing := relayPayAlipayFixture(t, keys, relayPayRoundTrip(func(*http.Request) (*http.Response, error) {
			return nil, io.ErrUnexpectedEOF
		}))
		_, err := failing.Query(context.Background(), order)
		assert.Error(t, err)
	})
}

func relaySignJSON(t *testing.T, key *rsa.PrivateKey, content string) string {
	return relayPaySign(t, key, content)
}

func TestRelayPayConfigAndTiers(t *testing.T) {
	keys := newRelayPayKeys(t)
	base := relayPayConfig{AppID: "2021000000000001", PrivateKey: base64.StdEncoding.EncodeToString(x509.MarshalPKCS1PrivateKey(keys.app)),
		AlipayPublicKey: relayPayPublicBase64(t, keys.alipay), NotifyURL: "https://pay.example.test/n", ConfigVersion: "v1"}
	for name, change := range map[string]func(*relayPayConfig){
		"http_notify": func(c *relayPayConfig) { c.NotifyURL = "http://pay.example.test/n" },
		"bad_app":     func(c *relayPayConfig) { c.AppID = "app" },
		"no_version":  func(c *relayPayConfig) { c.ConfigVersion = "" },
		"bad_public":  func(c *relayPayConfig) { c.AlipayPublicKey = "not-a-key" },
		"bad_private": func(c *relayPayConfig) { c.PrivateKey = "not-a-key" },
		"http_return": func(c *relayPayConfig) { c.ReturnURL = "http://pay.example.test/r" },
	} {
		config := base
		change(&config)
		_, err := NewRelayPayAlipay(config, nil)
		assert.Error(t, err, name)
	}
	for _, amount := range []string{"0.01", "10.00", "100.00", "123456789.99"} {
		_, ok := relayPayFen(amount)
		assert.True(t, ok, amount)
	}
	for _, amount := range []string{"10", "10.0", "-1.00", "01.00", "1e3", "10.00 ", ""} {
		_, ok := relayPayFen(amount)
		assert.False(t, ok, amount)
	}
	assert.Equal(t, []int64{10, 30, 50, 100}, RelayPayTiers)
	_, err := NewRelayPayOrder(1, 20, relayPayMock{})
	assert.ErrorIs(t, err, model.ErrRelayInput, "only the confirmed tiers can be ordered")
	assert.Equal(t, "10.05", relayPayAmount(1005))
}
