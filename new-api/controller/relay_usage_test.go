package controller

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRelayUserUsageAPI(t *testing.T) {
	f := newRelayPayControllerFixture(t, "mock")
	a, b := f.users[0], f.users[1]
	now := time.Now().Unix()
	for i, receipt := range []model.RelayWalletReceipt{
		{UserID: a.Id, CreatedAt: now - 10, Actual: 170},
		{UserID: a.Id, CreatedAt: now - 5, Actual: 340},
		{UserID: a.Id, CreatedAt: now - 31*86400, Actual: 999}, // outside 30 days
		{UserID: b.Id, CreatedAt: now - 1, Actual: 77},
	} {
		receipt.RequestID = fmt.Sprintf("00000000-0000-4000-8000-%012x", i+1)
		receipt.TokenID, receipt.ChannelID, receipt.Model, receipt.PriceVersion = 9, 2, "deepseek-v4-flash", "fixture-secret-version"
		receipt.Input, receipt.CacheRead, receipt.Output = 100, 10, 20
		require.NoError(t, model.DB.Create(&receipt).Error)
	}
	identity := service.AuthIdentity{UserID: 1}
	get := func(id, query string) (int, map[string]any) {
		w := adminUserRequest("GET", "/usage"+query, "", "", identity, common.RoleAdminUser, gin.Params{{Key: "id", Value: id}}, GetRelayWalletUserUsage)
		var data map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &data)
		return w.Code, data
	}
	status, data := get(fmt.Sprint(a.Id), "")
	require.Equal(t, 200, status)
	page := data["data"].(map[string]any)
	require.Equal(t, float64(2), page["total"])
	items := page["items"].([]any)
	require.Len(t, items, 2)
	first := items[0].(map[string]any)
	require.Equal(t, float64(340), first["actual"], "newest first")
	require.Equal(t, "00000000-0000-4000-8000-000000000002", first["request_id"])
	raw, _ := json.Marshal(data)
	for _, hidden := range []string{"fixture-secret-version", "channel", "token_id", "price_version"} {
		require.NotContains(t, string(raw), hidden)
	}
	status, data = get(fmt.Sprint(b.Id), "?p=1")
	require.Equal(t, 200, status)
	require.Equal(t, float64(1), data["data"].(map[string]any)["total"], "other user's receipts never mix in")
	for _, bad := range [][2]string{{"0", ""}, {"x", ""}, {fmt.Sprint(a.Id), "?p=0"}, {fmt.Sprint(a.Id), "?p=01"}, {fmt.Sprint(a.Id), "?p=1&p=2"}, {fmt.Sprint(a.Id), "?days=90"}} {
		status, _ = get(bad[0], bad[1])
		require.Equal(t, 400, status, strings.Join(bad[:], ""))
	}
	t.Setenv("RELAY_WALLET_SCHEMA", "false")
	status, _ = get(fmt.Sprint(a.Id), "")
	require.Equal(t, 503, status)
}
