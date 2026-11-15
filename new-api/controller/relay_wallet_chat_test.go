package controller

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const relayWalletChatUsage = `{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150,"prompt_cache_hit_tokens":40}`

// relayWalletChatUpstream is a local OpenAI-compatible provider. It never
// reaches a real model; each response mode is chosen by the test.
type relayWalletChatUpstream struct {
	mu       sync.Mutex
	mode     string
	bodies   []map[string]any
	received atomic.Int32
	release  chan struct{}
	started  chan struct{}
}

func (upstream *relayWalletChatUpstream) serve(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		upstream.received.Add(1)
		assert.Equal(t, "/v1/chat/completions", r.URL.Path)
		assert.Equal(t, "Bearer sk-upstream-fixture", r.Header.Get("Authorization"))
		var body map[string]any
		assert.NoError(t, common.DecodeJson(r.Body, &body))
		upstream.mu.Lock()
		upstream.bodies = append(upstream.bodies, body)
		mode, release, started := upstream.mode, upstream.release, upstream.started
		upstream.mu.Unlock()
		chunk := func(choices, extra string) string {
			return `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"vendor/chat-model","choices":` + choices + extra + "}\n\n"
		}
		switch mode {
		case "json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","created":1,"model":"vendor/chat-model","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":`+relayWalletChatUsage+`}`)
			return
		case "rate_limited":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"slow down","type":"rate_limit"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		// The stock handler forwards a chunk once the next one arrives.
		_, _ = io.WriteString(w, chunk(`[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]`, ""))
		_, _ = io.WriteString(w, chunk(`[{"index":0,"delta":{"content":" world"},"finish_reason":null}]`, ""))
		flusher.Flush()
		if started != nil {
			close(started)
		}
		if release != nil {
			<-release
		}
		_, _ = io.WriteString(w, chunk(`[{"index":0,"delta":{"content":"!"},"finish_reason":null}]`, ""))
		_, _ = io.WriteString(w, chunk(`[{"index":0,"delta":{},"finish_reason":"stop"}]`, ""))
		if mode != "missing_usage" {
			_, _ = io.WriteString(w, chunk(`[]`, `,"usage":`+relayWalletChatUsage))
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}
}

type relayWalletChatFixture struct {
	upstream *relayWalletChatUpstream
	gateway  string
	user     *model.User
	token    *model.Token
	channel  *model.Channel
}

func newRelayWalletChatFixture(t *testing.T, paramOverride string) *relayWalletChatFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousType, previousLogType := common.MainDatabaseType(), common.LogDatabaseType()
	previousRedis, previousBatch := common.RedisEnabled, common.BatchUpdateEnabled
	previousMaster, previousMemory, previousSQLite := common.IsMasterNode, common.MemoryCacheEnabled, common.SQLitePath
	previousLogs, previousTimeout := common.LogConsumeEnabled, constant.StreamingTimeout
	t.Setenv("SQL_DSN", "")
	t.Setenv("LOG_SQL_DSN", "")
	t.Setenv("RELAY_WALLET_SCHEMA", "true")
	t.Setenv("RELAY_WALLET_BILLING", "true")
	common.IsMasterNode, common.MemoryCacheEnabled, common.RedisEnabled, common.BatchUpdateEnabled = false, false, false, false
	common.LogConsumeEnabled, constant.StreamingTimeout = true, 30
	common.SQLitePath = filepath.Join(t.TempDir(), "relay-wallet-chat.db")
	require.NoError(t, model.InitDB())
	sqlDB, err := model.DB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, model.InitLogDB())
	saved := map[string]string{}
	require.NoError(t, config.GlobalConfig.SaveToDB(func(key, value string) error {
		if key == "group_ratio_setting.group_ratio" {
			saved[key] = value
		}
		return nil
	}))
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"group_ratio_setting.group_ratio": `{"default":1}`}))
	t.Cleanup(func() {
		require.NoError(t, config.GlobalConfig.LoadFromDB(saved))
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.SetDatabaseTypes(previousType, previousLogType)
		common.RedisEnabled, common.BatchUpdateEnabled = previousRedis, previousBatch
		common.IsMasterNode, common.MemoryCacheEnabled, common.SQLitePath = previousMaster, previousMemory, previousSQLite
		common.LogConsumeEnabled, constant.StreamingTimeout = previousLogs, previousTimeout
		require.NoError(t, sqlDB.Close())
	})
	require.NoError(t, model.DB.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.Ability{}))
	require.NoError(t, model.LOG_DB.AutoMigrate(&model.Log{}))
	require.NoError(t, model.MigrateRelayWallet(model.DB))

	upstream := &relayWalletChatUpstream{mode: "stream"}
	provider := httptest.NewServer(upstream.serve(t))
	t.Cleanup(provider.Close)
	mapping := `{"chat-model":"vendor/chat-model"}`
	channel := &model.Channel{Name: "wallet-chat-upstream", Key: "sk-upstream-fixture", Status: common.ChannelStatusEnabled, Type: constant.ChannelTypeSiliconFlow,
		Group: "default", Models: "chat-model,unpriced-model", BaseURL: &provider.URL, ModelMapping: &mapping}
	if paramOverride != "" {
		channel.ParamOverride = &paramOverride
	}
	require.NoError(t, model.DB.Create(channel).Error)
	for _, name := range []string{"chat-model", "unpriced-model"} {
		require.NoError(t, model.DB.Create(&model.Ability{ChannelId: channel.Id, Model: name, Group: "default", Enabled: true}).Error)
	}
	prices := filepath.Join(t.TempDir(), "prices.json")
	require.NoError(t, os.WriteFile(prices, []byte(fmt.Sprintf(`{"routes":[{"channel_id":%d,"request_model":"chat-model","output_cap":"max_tokens_total","thinking":"disabled",`+
		`"schema":"text-v1","version":"fixture-2026-10-27","model":"vendor/chat-model","unit":"new-api-quota","input_rate":"1000000",`+
		`"cache_read_rate":"250000","output_rate":"2000000","context_tokens":4000,"input_limit":1000,"output_limit":500,`+
		`"upstream_input_rate":"600000","upstream_cache_read_rate":"150000","upstream_output_rate":"1300000"}]}`, channel.Id)), 0o600))
	t.Setenv("RELAY_WALLET_PRICE_FILE", prices)

	user := &model.User{Username: "wallet-chat-user", AffCode: "wallet-chat-user", Password: "fixture-only", Role: common.RoleCommonUser,
		Status: common.UserStatusEnabled, Group: "default", Quota: 10000, AuthVersion: 1}
	require.NoError(t, model.DB.Create(user).Error)
	token := &model.Token{UserId: user.Id, Key: strings.ReplaceAll(uuid.NewString(), "-", ""), Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 5000}
	require.NoError(t, model.DB.Create(token).Error)

	engine := gin.New()
	handler := func(format types.RelayFormat) gin.HandlerFunc {
		return func(c *gin.Context) {
			c.Set(common.RequestIdKey, "relay-wallet-chat")
			Relay(c, format)
		}
	}
	engine.POST("/v1/chat/completions", middleware.TokenAuth(), middleware.Distribute(), handler(types.RelayFormatOpenAI))
	engine.POST("/v1/embeddings", middleware.TokenAuth(), middleware.Distribute(), handler(types.RelayFormatEmbedding))
	gateway := httptest.NewServer(engine)
	t.Cleanup(gateway.Close)
	return &relayWalletChatFixture{upstream: upstream, gateway: gateway.URL, user: user, token: token, channel: channel}
}

func (fixture *relayWalletChatFixture) post(t *testing.T, path, body string, headers map[string]string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, fixture.gateway+path, strings.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer sk-"+fixture.token.Key)
	request.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	return response
}

func (fixture *relayWalletChatFixture) requests(t *testing.T) []model.RelayWalletRequest {
	t.Helper()
	var requests []model.RelayWalletRequest
	require.NoError(t, model.DB.Where("user_id = ?", fixture.user.Id).Order("created_at").Find(&requests).Error)
	return requests
}

func (fixture *relayWalletChatFixture) waitState(t *testing.T, id, state string) {
	t.Helper()
	require.Eventually(t, func() bool {
		var request model.RelayWalletRequest
		return model.DB.Where("id = ?", id).Take(&request).Error == nil && request.State == state
	}, 10*time.Second, 20*time.Millisecond, "request %s did not reach %s", id, state)
}

func (fixture *relayWalletChatFixture) balances(t *testing.T) (int, int, int) {
	t.Helper()
	var user model.User
	require.NoError(t, model.DB.First(&user, fixture.user.Id).Error)
	var token model.Token
	require.NoError(t, model.DB.First(&token, fixture.token.Id).Error)
	return user.Quota, token.RemainQuota, user.UsedQuota
}

const relayWalletChatStream = `{"model":"chat-model","stream":true,"messages":[{"role":"user","content":"hello"}]}`

func TestRelayWalletChatEndToEnd(t *testing.T) {
	t.Run("stream_settles_trusted_usage_once", func(t *testing.T) {
		fixture := newRelayWalletChatFixture(t, "")
		response := fixture.post(t, "/v1/chat/completions", relayWalletChatStream, nil)
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		require.Equal(t, http.StatusOK, response.StatusCode, string(body))
		assert.Contains(t, string(body), "Hello")
		assert.Contains(t, string(body), "[DONE]")
		id := response.Header.Get("X-Relay-Request-Id")
		require.NotEmpty(t, id)
		fixture.waitState(t, id, "settled")
		quota, remain, used := fixture.balances(t)
		assert.Equal(t, 10000-170, quota)
		assert.Equal(t, 5000-170, remain)
		assert.Equal(t, 170, used, "stock post-consumption must not charge again")

		require.Len(t, fixture.upstream.bodies, 1)
		sent := fixture.upstream.bodies[0]
		assert.Equal(t, "vendor/chat-model", sent["model"])
		assert.EqualValues(t, 500, sent["max_tokens"], "the server forces the published total output cap")
		assert.Equal(t, map[string]any{"include_usage": true}, sent["stream_options"])
		assert.Equal(t, false, sent["enable_thinking"], "reasoning is forced off so max_tokens bounds all output")
		var receipt model.RelayWalletReceipt
		require.NoError(t, model.DB.Where("request_id = ?", id).Take(&receipt).Error)
		assert.EqualValues(t, 170, receipt.Actual)
		assert.Equal(t, "fixture-2026-10-27", receipt.PriceVersion)
		assert.Equal(t, fixture.channel.Id, receipt.ChannelID)
		var cost model.RelayWalletCost
		require.NoError(t, model.DB.Where("request_id = ?", id).Take(&cost).Error)
		assert.EqualValues(t, 107, cost.Actual, "provider cost is separate from the 170 quota sale")
		assert.Equal(t, receipt.PriceVersion, cost.PriceVersion)
		assert.Equal(t, receipt.CreatedAt, cost.CreatedAt)

		var logs int64
		require.NoError(t, model.LOG_DB.Model(&model.Log{}).Where("user_id = ? AND type = ?", fixture.user.Id, model.LogTypeConsume).Count(&logs).Error)
		assert.Zero(t, logs, "the stock consume log must not be written")
		delivered, err := model.SyncRelayWalletLogs(model.DB, model.LOG_DB, 100)
		require.NoError(t, err)
		assert.Equal(t, 1, delivered)
		require.NoError(t, model.LOG_DB.Model(&model.Log{}).Where("user_id = ? AND type = ? AND quota = ?", fixture.user.Id, model.LogTypeConsume, 170).Count(&logs).Error)
		assert.EqualValues(t, 1, logs)
	})

	t.Run("json_with_smaller_client_cap", func(t *testing.T) {
		fixture := newRelayWalletChatFixture(t, "")
		fixture.upstream.mode = "json"
		response := fixture.post(t, "/v1/chat/completions", `{"model":"chat-model","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`, nil)
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		require.Equal(t, http.StatusOK, response.StatusCode, string(body))
		requests := fixture.requests(t)
		require.Len(t, requests, 1)
		assert.EqualValues(t, 1000+200, requests[0].Reserved, "the smaller client cap lowers the hold")
		var costBasis model.RelayWalletCostSnapshot
		require.NoError(t, model.DB.Where("request_id = ?", requests[0].ID).Take(&costBasis).Error)
		assert.EqualValues(t, 100, costBasis.OutputLimit)
		assert.EqualValues(t, 730, costBasis.Reserved)
		fixture.waitState(t, requests[0].ID, "settled")
		assert.EqualValues(t, 100, fixture.upstream.bodies[0]["max_tokens"])
		quota, _, _ := fixture.balances(t)
		assert.Equal(t, 10000-170, quota)
	})

	t.Run("client_stop_still_settles_provider_usage", func(t *testing.T) {
		fixture := newRelayWalletChatFixture(t, "")
		fixture.upstream.release, fixture.upstream.started = make(chan struct{}), make(chan struct{})
		response := fixture.post(t, "/v1/chat/completions", relayWalletChatStream, nil)
		require.Equal(t, http.StatusOK, response.StatusCode)
		id := response.Header.Get("X-Relay-Request-Id")
		line, err := bufio.NewReader(response.Body).ReadString('\n')
		require.NoError(t, err)
		assert.Contains(t, line, "Hello")
		<-fixture.upstream.started
		require.NoError(t, response.Body.Close()) // The user pressed stop.
		time.Sleep(200 * time.Millisecond)
		fixture.waitState(t, id, "submitted")
		close(fixture.upstream.release)
		fixture.waitState(t, id, "settled")
		quota, _, _ := fixture.balances(t)
		assert.Equal(t, 10000-170, quota)
		assert.EqualValues(t, 1, fixture.upstream.received.Load(), "a stop must never trigger a new generation")
	})

	t.Run("missing_usage_holds_funds_as_unknown", func(t *testing.T) {
		fixture := newRelayWalletChatFixture(t, "")
		fixture.upstream.mode = "missing_usage"
		response := fixture.post(t, "/v1/chat/completions", relayWalletChatStream, nil)
		_, _ = io.Copy(io.Discard, response.Body)
		require.NoError(t, response.Body.Close())
		id := response.Header.Get("X-Relay-Request-Id")
		fixture.waitState(t, id, "unknown")
		quota, remain, used := fixture.balances(t)
		hold, err := model.GetRelayWalletHold(model.DB, fixture.user.Id)
		require.NoError(t, err)
		assert.Equal(t, model.RelayWalletHold{Held: 2000, Active: 1, Unknown: 1}, hold, "held funds are reported separately from the available balance")
		assert.Equal(t, 10000-2000, quota, "the full hold stays until review")
		assert.Equal(t, 5000-2000, remain)
		assert.Zero(t, used)
	})

	t.Run("explicit_rejection_settles_at_zero", func(t *testing.T) {
		fixture := newRelayWalletChatFixture(t, "")
		fixture.upstream.mode = "rate_limited"
		response := fixture.post(t, "/v1/chat/completions", relayWalletChatStream, nil)
		_, _ = io.Copy(io.Discard, response.Body)
		require.NoError(t, response.Body.Close())
		assert.NotEqual(t, http.StatusOK, response.StatusCode)
		requests := fixture.requests(t)
		require.Len(t, requests, 1)
		fixture.waitState(t, requests[0].ID, "settled")
		quota, remain, _ := fixture.balances(t)
		assert.Equal(t, 10000, quota)
		assert.Equal(t, 5000, remain)
		assert.EqualValues(t, 1, fixture.upstream.received.Load(), "no channel retry after a dispatched request")
	})

	t.Run("override_widening_output_is_refused_before_dispatch", func(t *testing.T) {
		fixture := newRelayWalletChatFixture(t, `{"max_tokens":5000}`)
		response := fixture.post(t, "/v1/chat/completions", relayWalletChatStream, nil)
		_, _ = io.Copy(io.Discard, response.Body)
		require.NoError(t, response.Body.Close())
		assert.NotEqual(t, http.StatusOK, response.StatusCode)
		assert.Zero(t, fixture.upstream.received.Load())
		requests := fixture.requests(t)
		require.Len(t, requests, 1)
		assert.Equal(t, "released", requests[0].State)
		quota, remain, _ := fixture.balances(t)
		assert.Equal(t, 10000, quota)
		assert.Equal(t, 5000, remain)
	})

	t.Run("duplicate_request_id_never_dispatches_again", func(t *testing.T) {
		fixture := newRelayWalletChatFixture(t, "")
		id := uuid.NewString()
		headers := map[string]string{"X-Relay-Request-Id": id}
		response := fixture.post(t, "/v1/chat/completions", relayWalletChatStream, headers)
		_, _ = io.Copy(io.Discard, response.Body)
		require.NoError(t, response.Body.Close())
		require.Equal(t, http.StatusOK, response.StatusCode)
		fixture.waitState(t, id, "settled")
		for _, body := range []string{relayWalletChatStream, strings.Replace(relayWalletChatStream, "hello", "changed", 1)} {
			replay := fixture.post(t, "/v1/chat/completions", body, headers)
			_, _ = io.Copy(io.Discard, replay.Body)
			require.NoError(t, replay.Body.Close())
			assert.Equal(t, http.StatusConflict, replay.StatusCode)
		}
		assert.EqualValues(t, 1, fixture.upstream.received.Load())
		quota, _, _ := fixture.balances(t)
		assert.Equal(t, 10000-170, quota)
		invalid := fixture.post(t, "/v1/chat/completions", relayWalletChatStream, map[string]string{"X-Relay-Request-Id": "not-a-uuid"})
		_, _ = io.Copy(io.Discard, invalid.Body)
		require.NoError(t, invalid.Body.Close())
		assert.Equal(t, http.StatusBadRequest, invalid.StatusCode)
	})

	t.Run("second_generation_for_same_user_is_rejected", func(t *testing.T) {
		fixture := newRelayWalletChatFixture(t, "")
		fixture.upstream.release, fixture.upstream.started = make(chan struct{}), make(chan struct{})
		first := fixture.post(t, "/v1/chat/completions", relayWalletChatStream, nil)
		require.Equal(t, http.StatusOK, first.StatusCode)
		<-fixture.upstream.started
		fixture.upstream.mu.Lock()
		release := fixture.upstream.release
		fixture.upstream.release, fixture.upstream.started = nil, nil
		fixture.upstream.mu.Unlock()
		second := fixture.post(t, "/v1/chat/completions", strings.Replace(relayWalletChatStream, "hello", "again", 1), nil)
		body, err := io.ReadAll(second.Body)
		require.NoError(t, err)
		require.NoError(t, second.Body.Close())
		assert.Equal(t, http.StatusTooManyRequests, second.StatusCode, string(body))
		assert.Contains(t, string(body), "relay_wallet_user_busy")
		assert.EqualValues(t, 1, fixture.upstream.received.Load())
		close(release)
		_, _ = io.Copy(io.Discard, first.Body)
		require.NoError(t, first.Body.Close())
		fixture.waitState(t, first.Header.Get("X-Relay-Request-Id"), "settled")
		third := fixture.post(t, "/v1/chat/completions", strings.Replace(relayWalletChatStream, "hello", "after", 1), nil)
		_, _ = io.Copy(io.Discard, third.Body)
		require.NoError(t, third.Body.Close())
		assert.Equal(t, http.StatusOK, third.StatusCode, "settlement frees the user's generation slot")
		fixture.waitState(t, third.Header.Get("X-Relay-Request-Id"), "settled")
	})

	t.Run("unsupported_routes_and_models_never_charge", func(t *testing.T) {
		fixture := newRelayWalletChatFixture(t, "")
		embedding := fixture.post(t, "/v1/embeddings", `{"model":"chat-model","input":"hello"}`, nil)
		_, _ = io.Copy(io.Discard, embedding.Body)
		require.NoError(t, embedding.Body.Close())
		assert.Equal(t, http.StatusForbidden, embedding.StatusCode)
		unpriced := fixture.post(t, "/v1/chat/completions", strings.Replace(relayWalletChatStream, "chat-model", "unpriced-model", 1), nil)
		_, _ = io.Copy(io.Discard, unpriced.Body)
		require.NoError(t, unpriced.Body.Close())
		assert.Equal(t, http.StatusBadRequest, unpriced.StatusCode)
		tooLong := fixture.post(t, "/v1/chat/completions", strings.Replace(relayWalletChatStream, "hello", strings.Repeat("hello ", 3000), 1), nil)
		_, _ = io.Copy(io.Discard, tooLong.Body)
		require.NoError(t, tooLong.Body.Close())
		assert.Equal(t, http.StatusRequestEntityTooLarge, tooLong.StatusCode)
		tools := fixture.post(t, "/v1/chat/completions", strings.Replace(relayWalletChatStream, `"stream":true`, `"stream":true,"n":2`, 1), nil)
		_, _ = io.Copy(io.Discard, tools.Body)
		require.NoError(t, tools.Body.Close())
		assert.Equal(t, http.StatusBadRequest, tools.StatusCode)
		assert.Empty(t, fixture.requests(t))
		assert.Zero(t, fixture.upstream.received.Load())
		quota, remain, _ := fixture.balances(t)
		assert.Equal(t, 10000, quota)
		assert.Equal(t, 5000, remain)
	})
}

func TestRelayWalletAdminRoutesDeclared(t *testing.T) {
	// Admin access tokens fail closed on undeclared routes; the identity service
	// reads holds with the same scope it already uses for GET /api/user/:id.
	hold, ok := middleware.AccessTokenRouteRule("GET /api/relay_wallet/user/:id")
	require.True(t, ok)
	user, ok := middleware.AccessTokenRouteRule("GET /api/user/:id")
	require.True(t, ok)
	assert.Equal(t, user, hold)
}
