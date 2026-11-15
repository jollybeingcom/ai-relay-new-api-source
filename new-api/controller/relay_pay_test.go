package controller

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type relayPayFixture struct {
	gateway string
	alipay  *rsa.PrivateKey
	users   []*model.User
}

func newRelayPayControllerFixture(t *testing.T, channel string) *relayPayFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousType, previousLogType := common.MainDatabaseType(), common.LogDatabaseType()
	previousRedis, previousBatch := common.RedisEnabled, common.BatchUpdateEnabled
	previousMaster, previousMemory, previousSQLite := common.IsMasterNode, common.MemoryCacheEnabled, common.SQLitePath
	t.Setenv("SQL_DSN", "")
	t.Setenv("LOG_SQL_DSN", "")
	t.Setenv("RELAY_WALLET_SCHEMA", "true")
	common.IsMasterNode, common.MemoryCacheEnabled, common.RedisEnabled, common.BatchUpdateEnabled = false, false, false, false
	common.SQLitePath = filepath.Join(t.TempDir(), "relay-pay.db")
	require.NoError(t, model.InitDB())
	sqlDB, err := model.DB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, model.InitLogDB())
	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.SetDatabaseTypes(previousType, previousLogType)
		common.RedisEnabled, common.BatchUpdateEnabled = previousRedis, previousBatch
		common.IsMasterNode, common.MemoryCacheEnabled, common.SQLitePath = previousMaster, previousMemory, previousSQLite
		require.NoError(t, sqlDB.Close())
	})
	require.NoError(t, model.DB.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}))
	require.NoError(t, model.MigrateRelayWallet(model.DB))

	fixture := &relayPayFixture{}
	for i := range 2 {
		user := &model.User{Username: fmt.Sprintf("payer%d", i), AffCode: fmt.Sprintf("payer%d", i), Password: "fixture-only",
			Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: "default"}
		require.NoError(t, model.DB.Create(user).Error)
		fixture.users = append(fixture.users, user)
	}
	t.Setenv("RELAY_PAY_CHANNEL", channel)
	if channel == "alipay" {
		app, err := rsa.GenerateKey(rand.Reader, 2048)
		require.NoError(t, err)
		fixture.alipay, err = rsa.GenerateKey(rand.Reader, 2048)
		require.NoError(t, err)
		public, err := x509.MarshalPKIXPublicKey(&fixture.alipay.PublicKey)
		require.NoError(t, err)
		config, err := json.Marshal(map[string]any{"app_id": "2021000000000001", "private_key": base64.StdEncoding.EncodeToString(x509.MarshalPKCS1PrivateKey(app)),
			"alipay_public_key": base64.StdEncoding.EncodeToString(public), "notify_url": "https://pay.example.test/api/relay_pay/notify/alipay",
			"config_version": "fixture-" + filepath.Base(t.TempDir())})
		require.NoError(t, err)
		path := filepath.Join(t.TempDir(), "alipay.json")
		require.NoError(t, os.WriteFile(path, config, 0o600))
		t.Setenv("RELAY_PAY_ALIPAY_FILE", path)
	}
	// Stand-in for UserAuth/AdminAuth, whose session checks are upstream's own.
	as := func(index int, role int) gin.HandlerFunc {
		return func(c *gin.Context) { c.Set("id", fixture.users[index].Id); c.Set("role", role); c.Next() }
	}
	engine := gin.New()
	for i := range fixture.users {
		group := engine.Group(fmt.Sprintf("/u%d", i), as(i, common.RoleCommonUser))
		group.POST("/orders", CreateRelayPayOrder)
		group.GET("/orders", ListRelayPayOrders)
		group.GET("/orders/:id", GetRelayPayOrder)
	}
	engine.POST("/admin/mock/:id/pay", as(0, common.RoleRootUser), MockRelayPayPaid)
	identity := engine.Group("/identity/:user", as(0, common.RoleRootUser), RelayPayAsUser)
	identity.POST("/orders", CreateRelayPayOrder)
	identity.GET("/orders", ListRelayPayOrders)
	identity.GET("/orders/:id", GetRelayPayOrder)
	identity.POST("/orders/:id/mock-pay", MockRelayPayPaidForUser)
	engine.POST("/api/relay_pay/notify/alipay", middleware.AnonymousRequestBodyLimit(), RelayPayAlipayNotify)
	server := httptest.NewServer(engine)
	t.Cleanup(server.Close)
	fixture.gateway = server.URL
	return fixture
}

func (f *relayPayFixture) call(t *testing.T, method, path, body string) (int, map[string]any) {
	t.Helper()
	request, err := http.NewRequest(method, f.gateway+path, strings.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	var result map[string]any
	require.NoError(t, json.NewDecoder(response.Body).Decode(&result))
	return response.StatusCode, result
}

func (f *relayPayFixture) quota(t *testing.T, index int) int {
	var user model.User
	require.NoError(t, model.DB.First(&user, f.users[index].Id).Error)
	return user.Quota
}

func (f *relayPayFixture) notify(t *testing.T, order, amount, trade string, sign bool) string {
	values := url.Values{"app_id": {"2021000000000001"}, "out_trade_no": {order}, "trade_no": {trade}, "total_amount": {amount},
		"trade_status": {"TRADE_SUCCESS"}, "notify_id": {"n-" + trade}, "charset": {"utf-8"}, "sign_type": {"RSA2"}}
	keys := make([]string, 0, len(values))
	for key := range values {
		if key != "sign_type" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+values.Get(key))
	}
	digest := sha256.Sum256([]byte(strings.Join(parts, "&")))
	signature, err := rsa.SignPKCS1v15(rand.Reader, f.alipay, crypto.SHA256, digest[:])
	require.NoError(t, err)
	values.Set("sign", base64.StdEncoding.EncodeToString(signature))
	if !sign {
		values.Set("total_amount", "100.00") // Changed after signing.
	}
	response, err := http.Post(f.gateway+"/api/relay_pay/notify/alipay", "application/x-www-form-urlencoded", strings.NewReader(values.Encode()))
	require.NoError(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return string(body)
}

func TestRelayPayEndToEnd(t *testing.T) {
	t.Run("alipay_notification_credits_exactly_once", func(t *testing.T) {
		f := newRelayPayControllerFixture(t, "alipay")
		status, result := f.call(t, http.MethodPost, "/u0/orders", `{"amount_cny":30,"device":"pc","quota":999999999,"amount_fen":1}`)
		require.Equal(t, http.StatusOK, status, result)
		data := result["data"].(map[string]any)
		order := data["order"].(map[string]any)
		id := order["id"].(string)
		assert.Equal(t, "30.00", order["amount_cny"], "client-supplied amounts and quota are ignored")
		assert.Contains(t, data["pay_url"], "https://openapi-sandbox.dl.alipaydev.com/gateway.do?", "non-production config uses the sandbox")
		status, _ = f.call(t, http.MethodGet, "/u1/orders/"+id, "")
		assert.Equal(t, http.StatusNotFound, status, "another user cannot read the order")

		assert.Equal(t, "fail", f.notify(t, id, "30.00", "2026102722009000001", false), "a tampered notification is rejected")
		assert.Equal(t, "fail", f.notify(t, id, "3.00", "2026102722009000001", true), "a signed but wrong amount is rejected")
		assert.Zero(t, f.quota(t, 0))

		var wg sync.WaitGroup
		replies := make([]string, 20)
		for i := range replies {
			wg.Add(1)
			go func(i int) { defer wg.Done(); replies[i] = f.notify(t, id, "30.00", "2026102722009000001", true) }(i)
		}
		wg.Wait()
		for _, reply := range replies {
			assert.Equal(t, "success", reply)
		}
		assert.Equal(t, 30*1000000, f.quota(t, 0), "20 concurrent duplicate notifications credit once")
		status, result = f.call(t, http.MethodGet, "/u0/orders/"+id, "")
		require.Equal(t, http.StatusOK, status)
		view := result["data"].(map[string]any)
		assert.Equal(t, "paid", view["state"])
		assert.Equal(t, true, view["credited"])
		_, hasTrade := view["trade_no"]
		assert.False(t, hasTrade, "owner view omits provider identifiers")
		assert.Zero(t, f.quota(t, 1))
	})

	t.Run("tiers_and_unpaid_limit_are_enforced_server_side", func(t *testing.T) {
		f := newRelayPayControllerFixture(t, "mock")
		for _, body := range []string{`{"amount_cny":20,"device":"pc"}`, `{"amount_cny":0,"device":"pc"}`, `{"amount_cny":30,"device":"tv"}`, `{"amount_cny":"30"}`} {
			status, _ := f.call(t, http.MethodPost, "/u0/orders", body)
			assert.Equal(t, http.StatusBadRequest, status, body)
		}
		for range 3 {
			status, _ := f.call(t, http.MethodPost, "/u0/orders", `{"amount_cny":10,"device":"mobile"}`)
			require.Equal(t, http.StatusOK, status)
		}
		status, _ := f.call(t, http.MethodPost, "/u0/orders", `{"amount_cny":10,"device":"mobile"}`)
		assert.Equal(t, http.StatusTooManyRequests, status)
		status, result := f.call(t, http.MethodGet, "/u0/orders", "")
		require.Equal(t, http.StatusOK, status)
		assert.EqualValues(t, 3, result["data"].(map[string]any)["total"])
		status, result = f.call(t, http.MethodGet, "/u1/orders", "")
		require.Equal(t, http.StatusOK, status)
		assert.EqualValues(t, 0, result["data"].(map[string]any)["total"], "users list only their own orders")
	})

	t.Run("mock_channel_pays_only_through_admin_action", func(t *testing.T) {
		f := newRelayPayControllerFixture(t, "mock")
		_, result := f.call(t, http.MethodPost, "/u0/orders", `{"amount_cny":10,"device":"pc"}`)
		data := result["data"].(map[string]any)
		id := data["order"].(map[string]any)["id"].(string)
		assert.Equal(t, "/relay-pay-mock?order="+id, data["pay_url"])
		status, _ := f.call(t, http.MethodGet, "/u0/orders/"+id, "")
		assert.Equal(t, http.StatusOK, status, "visiting the order (a return page) never credits")
		assert.Zero(t, f.quota(t, 0))
		assert.Equal(t, "fail", func() string {
			response, err := http.Post(f.gateway+"/api/relay_pay/notify/alipay", "application/x-www-form-urlencoded", strings.NewReader("out_trade_no="+id))
			require.NoError(t, err)
			defer response.Body.Close()
			body, _ := io.ReadAll(response.Body)
			return string(body)
		}(), "the Alipay endpoint is closed when another channel is active")
		for range 2 {
			status, _ = f.call(t, http.MethodPost, "/admin/mock/"+id+"/pay", "")
			require.Equal(t, http.StatusOK, status)
		}
		assert.Equal(t, 10*1000000, f.quota(t, 0))
		assert.True(t, (relayPayQueryHandler{}).Enabled())
	})

	t.Run("identity_acts_only_for_the_named_user", func(t *testing.T) {
		f := newRelayPayControllerFixture(t, "mock")
		first, second := fmt.Sprint(f.users[0].Id), fmt.Sprint(f.users[1].Id)
		status, result := f.call(t, http.MethodPost, "/identity/"+second+"/orders", `{"amount_cny":50,"device":"mobile"}`)
		require.Equal(t, http.StatusOK, status, result)
		id := result["data"].(map[string]any)["order"].(map[string]any)["id"].(string)
		status, _ = f.call(t, http.MethodGet, "/identity/"+first+"/orders/"+id, "")
		assert.Equal(t, http.StatusNotFound, status)
		status, _ = f.call(t, http.MethodPost, "/identity/"+first+"/orders/"+id+"/mock-pay", "")
		assert.Equal(t, http.StatusNotFound, status, "one user cannot pay or read another user's order")
		assert.Zero(t, f.quota(t, 1))
		status, _ = f.call(t, http.MethodPost, "/identity/"+second+"/orders/"+id+"/mock-pay", "")
		require.Equal(t, http.StatusOK, status)
		assert.Equal(t, 50*1000000, f.quota(t, 1))
		assert.Zero(t, f.quota(t, 0))
		admin := &model.User{Username: "root-target", AffCode: "root-target", Password: "fixture-only", Role: common.RoleRootUser, Status: common.UserStatusEnabled}
		require.NoError(t, model.DB.Create(admin).Error)
		for _, target := range []string{fmt.Sprint(admin.Id), "0", "abc", "999999"} {
			status, _ = f.call(t, http.MethodPost, "/identity/"+target+"/orders", `{"amount_cny":10,"device":"pc"}`)
			assert.Equal(t, http.StatusNotFound, status, target)
		}
	})
}
