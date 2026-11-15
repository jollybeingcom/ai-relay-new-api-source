package channel

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	appcommon "github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestDoRequestReturnsUpstreamRedirectWithoutFollowing(t *testing.T) {
	service.InitHttpClient()
	gin.SetMode(gin.TestMode)
	sharedClient := service.GetHttpClient()
	require.NotNil(t, sharedClient)
	require.NotNil(t, sharedClient.CheckRedirect)
	originalRedirectPolicy := reflect.ValueOf(sharedClient.CheckRedirect).Pointer()

	var targetRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetRequests.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))
	defer target.Close()

	const responseBody = "redirect response"
	tests := []int{
		http.StatusMovedPermanently,
		http.StatusFound,
		http.StatusSeeOther,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect,
	}

	for _, statusCode := range tests {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			targetRequests.Store(0)
			var sourceRequests atomic.Int32
			type sourceResult struct {
				body []byte
				err  error
			}
			sourceResultCh := make(chan sourceResult, 1)
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sourceRequests.Add(1)
				body, err := io.ReadAll(r.Body)
				sourceResultCh <- sourceResult{body: body, err: err}
				w.Header().Set("Location", target.URL+"/redirect-target")
				w.WriteHeader(statusCode)
				_, _ = io.WriteString(w, responseBody)
			}))
			defer source.Close()

			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/relay", nil)

			req, err := http.NewRequest(http.MethodPost, source.URL, bytes.NewReader([]byte("request body")))
			require.NoError(t, err)
			info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}}

			resp, err := doRequest(ctx, req, info)
			require.NoError(t, err)
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			gotSource := <-sourceResultCh
			require.NoError(t, gotSource.err)

			assert.Equal(t, statusCode, resp.StatusCode)
			assert.Equal(t, target.URL+"/redirect-target", resp.Header.Get("Location"))
			assert.Equal(t, responseBody, string(body))
			assert.Equal(t, []byte("request body"), gotSource.body)
			assert.EqualValues(t, 1, sourceRequests.Load())
			assert.Zero(t, targetRequests.Load())
		})
	}

	assert.Equal(t, originalRedirectPolicy, reflect.ValueOf(sharedClient.CheckRedirect).Pointer(), "the cached client must not be mutated")
}

func TestRelayWalletHTTPDispatch(t *testing.T) {
	service.InitHttpClient()
	previousType := appcommon.MainDatabaseType()
	previousRedis, previousBatch := appcommon.RedisEnabled, appcommon.BatchUpdateEnabled
	appcommon.SetMainDatabaseType(appcommon.DatabaseTypeSQLite)
	appcommon.RedisEnabled, appcommon.BatchUpdateEnabled = false, false
	t.Cleanup(func() {
		appcommon.SetMainDatabaseType(previousType)
		appcommon.RedisEnabled, appcommon.BatchUpdateEnabled = previousRedis, previousBatch
	})
	for _, outcome := range []string{"success", "disconnect", "redirect"} {
		t.Run(outcome, func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(1)
			t.Cleanup(func() { assert.NoError(t, sqlDB.Close()) })
			require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}))
			require.NoError(t, model.MigrateRelayWallet(db))
			user := model.User{Username: "fixture", AffCode: "fixture", Password: "fixture-only", Quota: 1000, Role: 1, Status: 1}
			require.NoError(t, db.Create(&user).Error)
			token := model.Token{UserId: user.Id, Key: uuid.NewString(), RemainQuota: 1000, Status: 1, ExpiredTime: -1}
			require.NoError(t, db.Create(&token).Error)
			intent := model.RelayWalletRequest{ID: uuid.NewString(), UserID: user.Id, TokenID: token.Id,
				PayloadHash: strings.Repeat("a", 64), PriceVersion: "fixture", PriceSnapshot: `{"input":1}`, Reserved: 700}
			channel := model.Channel{Name: "http-fixture"}
			require.NoError(t, db.Create(&channel).Error)
			price := service.RelayTextPrice{ChannelID: channel.Id, Schema: "text-v1", Version: "fixture", Model: "fixture", Unit: "new-api-quota", InputRate: "1000000", CacheReadRate: "250000", OutputRate: "2000000", ContextTokens: 500, InputLimit: 300, OutputLimit: 200}
			wallet, err := service.NewPricedRelayWalletSession(db, intent, price)
			require.NoError(t, err)
			var received atomic.Int32
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received.Add(1)
				assert.Equal(t, 1, r.ProtoMajor, "durable requests must avoid HTTP/2 transparent retry")
				assert.Empty(t, r.Header.Get("Idempotency-Key"))
				assert.Empty(t, r.Header.Get("X-Idempotency-Key"))
				_, readErr := io.ReadAll(r.Body)
				assert.NoError(t, readErr)
				if outcome == "disconnect" {
					hijacker, ok := w.(http.Hijacker)
					if !assert.True(t, ok) {
						return
					}
					conn, _, hijackErr := hijacker.Hijack()
					if assert.NoError(t, hijackErr) {
						assert.NoError(t, conn.Close())
					}
					return
				}
				if outcome == "redirect" {
					w.Header().Set("Location", "https://"+r.Host+"/redirect-target")
					w.WriteHeader(http.StatusTemporaryRedirect)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"fixture","object":"chat.completion","model":"fixture","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150,"prompt_cache_hit_tokens":40}}`)
			}))
			server.EnableHTTP2 = true
			server.StartTLS()
			defer server.Close()
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}, Billing: wallet}
			info.ChannelId = channel.Id + 1000
			info.ChannelSetting.TLSInsecureSkipVerify = true // Only this local self-signed fixture.
			info.ChannelSetting.HTTPProtocol = "auto"
			info.ChannelSetting.HTTP2ConnectionShards = 4
			newRequest := func(body string) *http.Request {
				req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(body))
				require.NoError(t, err)
				req.Header.Set("Idempotency-Key", "must-not-enable-replay")
				req.Header.Set("X-Idempotency-Key", "must-not-enable-replay")
				return req
			}
			const valid = `{"model":"fixture","max_tokens":200,"messages":[{"role":"user","content":"hi"}]}`
			_, err = doRequest(ctx, newRequest(valid), info)
			require.Error(t, err, "a different channel cannot use the reserved price")
			assert.Zero(t, received.Load())
			info.ChannelId = channel.Id
			response, err := doRequest(ctx, newRequest(valid), info)
			if outcome == "disconnect" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				_, err = io.ReadAll(response.Body)
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
				if outcome == "redirect" {
					assert.Equal(t, http.StatusTemporaryRedirect, response.StatusCode)
				}
			}
			assert.EqualValues(t, 1, received.Load())
			_, err = doRequest(ctx, newRequest(valid), info)
			require.Error(t, err)
			assert.EqualValues(t, 1, received.Load(), "duplicate dispatch must not reach the upstream")
			assert.Equal(t, "auto", info.ChannelSetting.HTTPProtocol, "channel settings must not be mutated")
			assert.Equal(t, 4, info.ChannelSetting.HTTP2ConnectionShards)
			wantState, wantBalance := "unknown", 300
			if outcome == "success" {
				// The observed provider body settled the request when it reached EOF.
				require.NoError(t, wallet.Finish())
				wantState, wantBalance = "settled", 830
			} else {
				wallet.Refund(ctx)
			}
			var request model.RelayWalletRequest
			require.NoError(t, db.Where("id = ?", intent.ID).Take(&request).Error)
			assert.Equal(t, wantState, request.State)
			require.NoError(t, db.First(&user, user.Id).Error)
			assert.Equal(t, wantBalance, user.Quota)
		})
	}
}
