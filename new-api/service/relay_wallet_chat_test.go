package service

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func relayWalletChatFixture(t *testing.T, stream bool) (*gorm.DB, *RelayWalletSession, string, *model.User) {
	t.Helper()
	previousType := common.MainDatabaseType()
	previousRedis, previousBatch := common.RedisEnabled, common.BatchUpdateEnabled
	common.SetMainDatabaseType(common.DatabaseTypeSQLite)
	common.RedisEnabled, common.BatchUpdateEnabled = false, false
	t.Cleanup(func() {
		common.SetMainDatabaseType(previousType)
		common.RedisEnabled, common.BatchUpdateEnabled = previousRedis, previousBatch
	})
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { assert.NoError(t, sqlDB.Close()) })
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}))
	require.NoError(t, model.MigrateRelayWallet(db))
	user := model.User{Username: "observer", AffCode: "observer", Password: "fixture-only", Quota: 1000, Role: common.RoleCommonUser, Status: common.UserStatusEnabled}
	require.NoError(t, db.Create(&user).Error)
	token := model.Token{UserId: user.Id, Key: uuid.NewString(), RemainQuota: 1000, Status: common.TokenStatusEnabled, ExpiredTime: -1}
	require.NoError(t, db.Create(&token).Error)
	channel := model.Channel{Name: "observer-fixture"}
	require.NoError(t, db.Create(&channel).Error)
	price := RelayTextPrice{ChannelID: channel.Id, Schema: "text-v1", Version: "fixture-v1", Model: "fixture-model", Unit: "new-api-quota",
		InputRate: "1000000", CacheReadRate: "250000", OutputRate: "2000000", ContextTokens: 500, InputLimit: 300, OutputLimit: 200}
	id := uuid.NewString()
	session, err := NewPricedRelayWalletSession(db, model.RelayWalletRequest{ID: id, UserID: user.Id, TokenID: token.Id, PayloadHash: strings.Repeat("b", 64)}, price)
	require.NoError(t, err)
	require.True(t, session.created)
	session.stream = stream
	return db, session, id, &user
}

func relayWalletState(t *testing.T, db *gorm.DB, id string, user *model.User) (string, int) {
	t.Helper()
	var request model.RelayWalletRequest
	require.NoError(t, db.Where("id = ?", id).Take(&request).Error)
	require.NoError(t, db.First(user, user.Id).Error)
	return request.State, user.Quota
}

const relayWalletUsage = `{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150,"prompt_cache_hit_tokens":40}`

func TestRelayWalletChatObserver(t *testing.T) {
	completeStream := "data: {\"id\":\"x\",\"model\":\"fixture-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"x\",\"model\":\"fixture-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		": keep-alive comment\n\n" +
		"data: {\"id\":\"x\",\"model\":\"fixture-model\",\"choices\":[],\"usage\":" + relayWalletUsage + "}\n\n" +
		"data: [DONE]\n\n"
	cases := []struct {
		name, body string
		stream     bool
		readAll    bool
		want       string
		balance    int
	}{
		{"stream_complete", completeStream, true, true, "settled", 830},
		{"stream_client_left_drains_usage", completeStream, true, false, "settled", 830},
		{"stream_missing_usage", strings.Replace(completeStream, `,"usage":`+relayWalletUsage, "", 1), true, true, "unknown", 300},
		{"stream_without_done", strings.TrimSuffix(completeStream, "data: [DONE]\n\n"), true, true, "unknown", 300},
		{"stream_without_finish", strings.Replace(completeStream, `"finish_reason":"stop"`, `"finish_reason":null`, 1), true, true, "unknown", 300},
		{"stream_model_changed", strings.Replace(completeStream, `"model":"fixture-model","choices":[]`, `"model":"other","choices":[]`, 1), true, true, "unknown", 300},
		{"stream_error_event", "data: {\"error\":{\"message\":\"boom\"}}\n\n" + completeStream, true, true, "unknown", 300},
		{"stream_data_after_done", completeStream + "data: {\"model\":\"fixture-model\"}\n\n", true, true, "unknown", 300},
		{"stream_truncated_event", completeStream + "data: {\"model\":", true, true, "unknown", 300},
		{"stream_usage_over_bound", strings.Replace(completeStream, relayWalletUsage, `{"prompt_tokens":100,"completion_tokens":201,"total_tokens":301}`, 1), true, true, "unknown", 300},
		{"json_complete", `{"id":"x","model":"fixture-model","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"length"}],"usage":` + relayWalletUsage + `}`, false, true, "settled", 830},
		{"json_client_left_drains_usage", `{"id":"x","model":"fixture-model","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":` + relayWalletUsage + `}`, false, false, "settled", 830},
		{"json_missing_finish", `{"id":"x","model":"fixture-model","choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}],"usage":` + relayWalletUsage + `}`, false, true, "unknown", 300},
		{"json_explicit_zero_usage_missing_total", `{"model":"fixture-model","choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":0}}`, false, true, "unknown", 300},
		{"json_invalid", `{"model":"fixture-model",`, false, true, "unknown", 300},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, session, id, user := relayWalletChatFixture(t, tc.stream)
			require.NoError(t, session.Submit())
			resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(iotest.OneByteReader(strings.NewReader(tc.body)))}
			session.ObserveResponse(resp)
			if tc.readAll {
				_, err := io.ReadAll(resp.Body)
				require.NoError(t, err)
			} else {
				_, err := resp.Body.Read(make([]byte, 7)) // The handler stops early, as when the client leaves.
				require.NoError(t, err)
			}
			_ = resp.Body.Close()
			_ = session.Finish() // Repeated finish is a no-op.
			state, balance := relayWalletState(t, db, id, user)
			assert.Equal(t, tc.want, state)
			assert.Equal(t, tc.balance, balance)
			var receipts int64
			require.NoError(t, db.Model(&model.RelayWalletReceipt{}).Where("request_id = ?", id).Count(&receipts).Error)
			assert.EqualValues(t, map[bool]int{true: 1, false: 0}[tc.want == "settled"], receipts)
		})
	}

	statuses := map[int]struct {
		state   string
		balance int
	}{
		http.StatusTooManyRequests: {"settled", 1000}, http.StatusBadRequest: {"settled", 1000}, http.StatusUnauthorized: {"settled", 1000},
		http.StatusInternalServerError: {"unknown", 300}, http.StatusBadGateway: {"unknown", 300}, http.StatusTemporaryRedirect: {"unknown", 300},
		http.StatusRequestTimeout: {"unknown", 300},
	}
	for status, want := range statuses {
		t.Run("status_"+http.StatusText(status), func(t *testing.T) {
			db, session, id, user := relayWalletChatFixture(t, false)
			require.NoError(t, session.Submit())
			session.UpstreamContext()
			resp := &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"rejected"}}`))}
			session.ObserveResponse(resp)
			_, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			state, balance := relayWalletState(t, db, id, user)
			assert.Equal(t, want.state, state)
			assert.Equal(t, want.balance, balance)
		})
	}
}

func TestRelayWalletChatOutbound(t *testing.T) {
	valid := `{"model":"fixture-model","max_tokens":200,"messages":[{"role":"user","content":"hi"}]}`
	validStream := `{"model":"fixture-model","max_tokens":120,"stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`
	cases := []struct {
		name, path, body string
		stream, ok       bool
	}{
		{"valid", "/v1/chat/completions", valid, false, true},
		{"valid_stream", "/v1/chat/completions", validStream, true, true},
		{"explicit_thinking_off", "/v1/chat/completions", strings.Replace(valid, `"max_tokens"`, `"enable_thinking":false,"max_tokens"`, 1), false, true},
		{"responses_path", "/v1/responses", valid, false, false},
		{"model_mapped_elsewhere", "/v1/chat/completions", strings.Replace(valid, "fixture-model", "other", 1), false, false},
		{"missing_output_cap", "/v1/chat/completions", strings.Replace(valid, `"max_tokens":200,`, "", 1), false, false},
		{"widened_output_cap", "/v1/chat/completions", strings.Replace(valid, `200`, `201`, 1), false, false},
		{"completion_tokens_override", "/v1/chat/completions", strings.Replace(valid, `"max_tokens":200`, `"max_tokens":200,"max_completion_tokens":5000`, 1), false, false},
		{"multiple_choices", "/v1/chat/completions", strings.Replace(valid, `"max_tokens"`, `"n":2,"max_tokens"`, 1), false, false},
		{"thinking_enabled", "/v1/chat/completions", strings.Replace(valid, `"max_tokens"`, `"enable_thinking":true,"max_tokens"`, 1), false, false},
		{"reasoning_effort", "/v1/chat/completions", strings.Replace(valid, `"max_tokens"`, `"reasoning_effort":"high","max_tokens"`, 1), false, false},
		{"tools", "/v1/chat/completions", strings.Replace(valid, `"max_tokens"`, `"tools":[{"type":"function","function":{"name":"x"}}],"max_tokens"`, 1), false, false},
		{"stream_mode_changed", "/v1/chat/completions", validStream, false, false},
		{"stream_without_usage", "/v1/chat/completions", strings.Replace(validStream, `,"stream_options":{"include_usage":true}`, "", 1), true, false},
		{"invalid_json", "/v1/chat/completions", `{"model":`, false, false},
	}
	t.Run("thinking_must_be_disabled_when_required", func(t *testing.T) {
		for body, ok := range map[string]bool{
			valid: false,
			strings.Replace(valid, `"max_tokens"`, `"enable_thinking":null,"max_tokens"`, 1):  false,
			strings.Replace(valid, `"max_tokens"`, `"enable_thinking":true,"max_tokens"`, 1):  false,
			strings.Replace(valid, `"max_tokens"`, `"enable_thinking":false,"max_tokens"`, 1): true,
		} {
			_, session, _, _ := relayWalletChatFixture(t, false)
			session.thinkingOff = true
			req, err := http.NewRequest(http.MethodPost, "https://upstream.invalid/v1/chat/completions", strings.NewReader(body))
			require.NoError(t, err)
			assert.Equal(t, ok, session.PrepareOutbound(req) == nil, body)
		}
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, session, _, _ := relayWalletChatFixture(t, tc.stream)
			req, err := http.NewRequest(http.MethodPost, "https://upstream.invalid"+tc.path, strings.NewReader(tc.body))
			require.NoError(t, err)
			err = session.PrepareOutbound(req)
			if !tc.ok {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			sent, err := io.ReadAll(req.Body)
			require.NoError(t, err)
			assert.Equal(t, tc.body, string(sent), "the checked bytes are exactly the bytes sent")
			assert.EqualValues(t, len(tc.body), req.ContentLength)
		})
	}
}

func TestRelayWalletChatRoutes(t *testing.T) {
	valid := `{"routes":[{"channel_id":7,"request_model":"chat","output_cap":"max_tokens_total","thinking":"none","schema":"text-v1","version":"v1","model":"vendor/chat","unit":"new-api-quota","input_rate":"1","cache_read_rate":"0.5","output_rate":"2","context_tokens":1000,"input_limit":600,"output_limit":400,"upstream_input_rate":"0.5","upstream_cache_read_rate":"0.25","upstream_output_rate":"1"}]}`
	cases := map[string]string{
		"valid":             valid,
		"unknown_field":     strings.Replace(valid, `"unit"`, `"surprise":1,"unit"`, 1),
		"unverified_cap":    strings.Replace(valid, `"max_tokens_total"`, `"max_tokens"`, 1),
		"missing_channel":   strings.Replace(valid, `"channel_id":7,`, "", 1),
		"bad_rate":          strings.Replace(valid, `"output_rate":"2"`, `"output_rate":"2e3"`, 1),
		"context_exceeded":  strings.Replace(valid, `"output_limit":400`, `"output_limit":401`, 1),
		"duplicate":         strings.Replace(valid, `}]}`, `},`+valid[11:len(valid)-2]+`]}`, 1),
		"empty":             `{"routes":[]}`,
		"thinking_unstated": strings.Replace(valid, `"thinking":"none",`, "", 1),
		"thinking_on":       strings.Replace(valid, `"thinking":"none"`, `"thinking":"enabled"`, 1),
		"below_cost":        strings.Replace(valid, `"upstream_output_rate":"1"`, `"upstream_output_rate":"2.5"`, 1),
		"missing_cost":      strings.Replace(valid, `,"upstream_input_rate":"0.5"`, "", 1),
		"worst_case_over_cap": strings.Replace(strings.Replace(valid, `"output_rate":"2"`, `"output_rate":"3000000000"`, 1),
			`"upstream_output_rate":"1"`, `"upstream_output_rate":"2600000000"`, 1),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "prices.json")
			require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
			routes, err := parseRelayWalletRoutes(path)
			if name != "valid" {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			route := routes[relayWalletRouteKey(7, "chat")]
			assert.Equal(t, "vendor/chat", route.Model)
			assert.Equal(t, 7, route.ChannelID)
		})
	}
	t.Run("publisher_script_format", func(t *testing.T) {
		// Exact output of scripts/publish-relay-prices.py for the verified source.
		published := `{"routes":[{"channel_id":3,"request_model":"deepseek-v4-flash","output_cap":"max_tokens_total","thinking":"disabled",` +
			`"schema":"text-v1","version":"deepseek-v4-flash-2026-10-27-a","model":"deepseek-ai/DeepSeek-V4-Flash","unit":"new-api-quota",` +
			`"input_rate":"4500000","cache_read_rate":"450000","output_rate":"13500000","upstream_input_rate":"3000000",` +
			`"upstream_cache_read_rate":"300000","upstream_output_rate":"9000000","context_tokens":6500,"input_limit":4500,"output_limit":2000}]}`
		path := filepath.Join(t.TempDir(), "prices.json")
		require.NoError(t, os.WriteFile(path, []byte(published), 0o600))
		routes, err := parseRelayWalletRoutes(path)
		require.NoError(t, err)
		route := routes[relayWalletRouteKey(3, "deepseek-v4-flash")]
		reserve, err := route.Reservation()
		require.NoError(t, err)
		assert.Equal(t, 47250, reserve, "at most 0.04725 CNY is held per request")
	})
	t.Run("published_cny_view", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "prices.json")
		content := strings.NewReplacer(`"input_rate":"1"`, `"input_rate":"2000000"`, `"cache_read_rate":"0.5"`, `"cache_read_rate":"500000"`,
			`"output_rate":"2"`, `"output_rate":"8000000"`).Replace(valid)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		t.Setenv("RELAY_WALLET_PRICE_FILE", path)
		t.Setenv("RELAY_WALLET_BILLING", "false")
		prices, err := PublishedRelayWalletPrices()
		require.NoError(t, err)
		assert.Empty(t, prices, "nothing is published while paid chat is off")
		t.Setenv("RELAY_WALLET_BILLING", "true")
		prices, err = PublishedRelayWalletPrices()
		require.NoError(t, err)
		require.Len(t, prices, 1)
		assert.Equal(t, RelayWalletPublicPrice{Model: "chat", UpstreamModel: "vendor/chat", Version: "v1", InputPerMillion: "2",
			CachePerMillion: "0.5", OutputPerMillion: "8", InputLimit: 600, OutputLimit: 400, MaxCharge: "0.0044"}, prices[0])
	})
}

func TestRelayWalletRouteGuard(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		enabled      bool
		role         int
		method, path string
		allowed      bool
	}{
		{false, common.RoleCommonUser, http.MethodPost, "/v1/embeddings", true},
		{true, common.RoleAdminUser, http.MethodPost, "/v1/embeddings", true},
		{true, common.RoleCommonUser, http.MethodPost, "/v1/chat/completions", true},
		{true, common.RoleCommonUser, http.MethodGet, "/v1/models", true},
		{true, common.RoleCommonUser, http.MethodGet, "/dashboard/billing/usage", true},
		{true, common.RoleCommonUser, http.MethodPost, "/v1/embeddings", false},
		{true, common.RoleCommonUser, http.MethodPost, "/v1/responses", false},
		{true, common.RoleCommonUser, http.MethodGet, "/v1/realtime", false},
		{true, common.RoleCommonUser, http.MethodPost, "/mj/submit/imagine", false},
		{true, common.RoleCommonUser, http.MethodPost, "/v1/video/generations", false},
	}
	for _, tc := range cases {
		t.Setenv("RELAY_WALLET_BILLING", map[bool]string{true: "true", false: "false"}[tc.enabled])
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(tc.method, tc.path, nil)
		assert.Equal(t, tc.allowed, AllowRelayWalletRoute(c, tc.role), "%v %d %s %s", tc.enabled, tc.role, tc.method, tc.path)
		assert.Equal(t, tc.enabled && tc.role == common.RoleCommonUser, relayWalletMandatory(c))
		assert.Equal(t, tc.enabled && tc.role == common.RoleCommonUser, RejectStockRelayBilling(c, nil) != nil, "stock billing must be refused for mandatory users")
	}
}
