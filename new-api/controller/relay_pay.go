package controller

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

func relayPayJSON(c *gin.Context, status int, data any, message string) {
	c.JSON(status, gin.H{"success": status == http.StatusOK, "message": message, "data": data})
}

// CreateRelayPayOrder: the signed-in user picks a tier; everything else is
// fixed by the server, including amount, quota, merchant and expiry.
func CreateRelayPayOrder(c *gin.Context) {
	var request struct {
		AmountCNY int64  `json:"amount_cny"`
		Device    string `json:"device"`
	}
	if err := common.DecodeJson(c.Request.Body, &request); err != nil || request.Device != "pc" && request.Device != "mobile" {
		relayPayJSON(c, http.StatusBadRequest, nil, "invalid top-up request")
		return
	}
	channel, err := service.CurrentRelayPayChannel()
	if err != nil {
		relayPayJSON(c, http.StatusServiceUnavailable, nil, "top-up is not available")
		return
	}
	order, err := service.NewRelayPayOrder(c.GetInt("id"), request.AmountCNY, channel)
	switch {
	case errors.Is(err, model.ErrRelayStopped):
		relayPayJSON(c, 503, nil, "new generation and top-ups are paused")
		return
	case errors.Is(err, model.ErrRelayPayLimit):
		relayPayJSON(c, http.StatusTooManyRequests, nil, "too many unpaid orders; pay or wait for them to expire")
		return
	case errors.Is(err, model.ErrRelayInput):
		relayPayJSON(c, http.StatusBadRequest, nil, "unsupported top-up amount")
		return
	case err != nil:
		relayPayJSON(c, http.StatusForbidden, nil, "account cannot top up")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	payURL, err := channel.Create(ctx, order, request.Device == "mobile")
	if err != nil {
		common.SysError(common.LogText("relay pay create failed (order=%s): %s", order.ID, err.Error()))
		relayPayJSON(c, http.StatusBadGateway, nil, "payment page unavailable; no money was taken")
		return
	}
	relayPayJSON(c, http.StatusOK, gin.H{"order": service.ViewRelayPayOrder(order), "pay_url": payURL}, "")
}

func ListRelayPayOrders(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("p", "1"))
	orders, total, err := model.ListRelayPayOrders(model.DB, c.GetInt("id"), page)
	if err != nil {
		relayPayJSON(c, http.StatusBadRequest, nil, "invalid page")
		return
	}
	views := make([]service.RelayPayOrderView, 0, len(orders))
	for _, order := range orders {
		views = append(views, service.ViewRelayPayOrder(order))
	}
	relayPayJSON(c, http.StatusOK, gin.H{"items": views, "total": total, "page": page, "page_size": 20}, "")
}

// GetRelayPayOrder only reads the owner's order. A pending order may trigger
// one rate-limited provider query; the browser return page never credits.
func GetRelayPayOrder(c *gin.Context) {
	order, err := model.GetRelayPayOrder(model.DB, c.GetInt("id"), c.Param("id"))
	if err != nil {
		relayPayJSON(c, http.StatusNotFound, nil, "order not found")
		return
	}
	if channel, err := service.CurrentRelayPayChannel(); err == nil && order.State == "pending" &&
		order.Channel == channel.Name() && order.MerchantApp == channel.App() && time.Now().Unix()-order.UpdatedAt >= 10 {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
		fact, queryErr := channel.Query(ctx, order)
		cancel()
		if queryErr == nil {
			if applied, applyErr := model.ApplyRelayPayFact(model.DB, fact); applyErr == nil {
				order = applied
			}
		}
		if order.State == "pending" {
			_ = model.ScheduleRelayPayQuery(model.DB, order)
		}
	}
	relayPayJSON(c, http.StatusOK, service.ViewRelayPayOrder(order), "")
}

// RelayPayAlipayNotify acknowledges with "success" only after the credit
// transaction commits. Rejections are logged for troubleshooting.
func RelayPayAlipayNotify(c *gin.Context) {
	channel, err := service.CurrentRelayPayChannel()
	if err != nil || channel.Name() != "alipay" {
		c.String(http.StatusOK, "fail")
		return
	}
	fact, err := channel.Verify(c.Request)
	if err == nil {
		_, err = model.ApplyRelayPayFact(model.DB, fact)
	}
	if err != nil {
		common.SysError(common.LogText("relay pay notification rejected (order=%s, ip=%s): %s",
			common.LocalLogPreview(c.Request.PostForm.Get("out_trade_no")), c.ClientIP(), err.Error()))
		c.String(http.StatusOK, "fail")
		return
	}
	c.String(http.StatusOK, "success")
}

// MockRelayPayPaid is an administrator-only stand-in for a provider success
// in isolated development. It is unavailable unless the mock channel is active.
func MockRelayPayPaid(c *gin.Context) {
	channel, err := service.CurrentRelayPayChannel()
	if err != nil || channel.Name() != "mock" {
		relayPayJSON(c, http.StatusNotFound, nil, "mock channel disabled")
		return
	}
	var order model.RelayPayOrder
	if err := model.DB.Where("id = ?", c.Param("id")).Take(&order).Error; err != nil {
		relayPayJSON(c, http.StatusNotFound, nil, "order not found")
		return
	}
	order, err = service.MockRelayPaySuccess(order)
	if err != nil {
		relayPayJSON(c, http.StatusConflict, nil, "mock payment rejected")
		return
	}
	relayPayJSON(c, http.StatusOK, service.ViewRelayPayOrder(order), "")
}

type relayPayQueryHandler struct{}

func (relayPayQueryHandler) Type() string { return model.SystemTaskTypeRelayPayQuery }
func (relayPayQueryHandler) Enabled() bool {
	if !common.GetEnvOrDefaultBool("RELAY_WALLET_SCHEMA", false) {
		return false
	}
	_, err := service.CurrentRelayPayChannel()
	return err == nil
}
func (relayPayQueryHandler) Interval() time.Duration { return 30 * time.Second }
func (relayPayQueryHandler) NewPayload() any         { return nil }
func (relayPayQueryHandler) Run(ctx context.Context, task *model.SystemTask, runnerID string) {
	if !(relayPayQueryHandler{}).Enabled() {
		finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusFailed, nil, errors.New("relay pay query disabled"))
		return
	}
	changed, err := service.SyncRelayPayOrders(ctx, 50)
	status := model.SystemTaskStatusSucceeded
	if err != nil {
		status = model.SystemTaskStatusFailed
	}
	finishSystemTaskHandler(task, runnerID, status, map[string]int{"changed": changed}, err)
}

var _ service.ScheduledSystemTaskHandler = relayPayQueryHandler{}

// relayPayAsUser lets the trusted identity service act for one verified,
// mapped common user with its admin token. Handlers then see only that user.
func relayPayAsUser(c *gin.Context) {
	target, err := strconv.Atoi(c.Param("user"))
	var user model.User
	if err != nil || target <= 0 || model.DB.Where("id = ? AND role = ?", target, common.RoleCommonUser).Take(&user).Error != nil {
		relayPayJSON(c, http.StatusNotFound, nil, "user not found")
		c.Abort()
		return
	}
	c.Set("id", target)
	c.Next()
}

var RelayPayAsUser gin.HandlerFunc = relayPayAsUser

// MockRelayPayPaidForUser is the owner-test variant of MockRelayPayPaid: it
// only pays the given user's own order and only while the mock channel is on.
func MockRelayPayPaidForUser(c *gin.Context) {
	order, err := model.GetRelayPayOrder(model.DB, c.GetInt("id"), c.Param("id"))
	if err != nil {
		relayPayJSON(c, http.StatusNotFound, nil, "order not found")
		return
	}
	order, err = service.MockRelayPaySuccess(order)
	if err != nil {
		relayPayJSON(c, http.StatusNotFound, nil, "mock channel disabled")
		return
	}
	relayPayJSON(c, http.StatusOK, service.ViewRelayPayOrder(order), "")
}
