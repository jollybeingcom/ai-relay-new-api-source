package controller

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

func RelayRiskAPIGate(c *gin.Context) {
	if !common.GetEnvOrDefaultBool("RELAY_WALLET_SCHEMA", false) || !common.GetEnvOrDefaultBool("RELAY_RISK_API", false) {
		relayPayJSON(c, 503, nil, "financial operations unavailable")
		c.Abort()
		return
	}
	if !relayRefundAdmin(c) {
		c.Abort()
		return
	}
	c.Next()
}

func relayRiskError(c *gin.Context, err error) {
	if errors.Is(err, model.ErrRelayStopped) {
		relayPayJSON(c, 409, nil, "accounting or loss limit prevents recovery")
		return
	}
	relayRefundError(c, err)
}

func GetRelayRisk(c *gin.Context) {
	month := c.Query("month")
	if month == "" {
		month = model.RelayRiskMonth(time.Now())
	}
	if len(c.Request.URL.Query()) > 1 || len(c.Request.URL.Query()["month"]) > 1 || (len(c.Request.URL.Query()) == 1 && !c.Request.URL.Query().Has("month")) {
		relayRiskError(c, model.ErrRelayInput)
		return
	}
	r, err := model.ReadRelayRisk(model.DB, month)
	if err != nil {
		relayRiskError(c, err)
		return
	}
	relayPayJSON(c, 200, r, "")
}

func ListRelayFixedCosts(c *gin.Context) {
	for key, values := range c.Request.URL.Query() {
		if (key != "month" && key != "p") || len(values) != 1 {
			relayRiskError(c, model.ErrRelayInput)
			return
		}
	}
	page, err := strconv.Atoi(c.DefaultQuery("p", "1"))
	if err != nil || page < 1 || page > 1000 {
		relayRiskError(c, model.ErrRelayInput)
		return
	}
	month := c.Query("month")
	if month == "" {
		month = model.RelayRiskMonth(time.Now())
	}
	if _, err := model.ReadRelayRisk(model.DB, month); err != nil {
		relayRiskError(c, err)
		return
	}
	replaced := model.DB.Model(&model.RelayFixedCost{}).Select("supersedes").Where("supersedes IS NOT NULL")
	query := model.DB.Model(&model.RelayFixedCost{}).Where("month = ? AND id NOT IN (?)", month, replaced)
	var count int64
	if err := query.Count(&count).Error; err != nil {
		relayRiskError(c, err)
		return
	}
	items := []model.RelayFixedCost{}
	if err := query.Order("category, id").Offset((page - 1) * 20).Limit(20).Find(&items).Error; err != nil {
		relayRiskError(c, err)
		return
	}
	relayPayJSON(c, 200, gin.H{"items": items, "total": count}, "")
}

func relayFinanceProof(c *gin.Context, action string, body any) bool {
	op, err := service.RelayFinanceOperation(action, body)
	if err != nil {
		relayRiskError(c, err)
		return false
	}
	query := c.Request.URL.Query()
	if len(query) != 0 {
		if len(query) == 1 && len(query["prepare"]) == 1 && query.Get("prepare") == "1" {
			relayPayJSON(c, 200, gin.H{"verification": op}, "")
			return false
		}
		relayRiskError(c, model.ErrRelayInput)
		return false
	}
	return middleware.RequireSecurityProof(c, op) != nil
}

type relayRiskControlInput struct {
	Revision int64  `json:"revision"`
	Action   string `json:"action"`
	Reason   string `json:"reason"`
}

func ControlRelayRisk(c *gin.Context) {
	var input relayRiskControlInput
	if relayRefundDecode(c, &input) != nil {
		relayRiskError(c, model.ErrRelayInput)
		return
	}
	if !relayFinanceProof(c, "risk_control", input) {
		return
	}
	state, err := model.ChangeRelayRisk(model.DB, c.GetInt("id"), input.Revision, input.Action, input.Reason)
	if err != nil {
		relayRiskError(c, err)
		return
	}
	relayPayJSON(c, 200, state, "")
}

type relayFixedCostInput struct {
	ID         string  `json:"id"`
	Month      string  `json:"month"`
	Category   string  `json:"category"`
	AmountFen  int64   `json:"amount_fen"`
	Estimated  *bool   `json:"estimated"`
	Supersedes *string `json:"supersedes"`
	Reason     string  `json:"reason"`
}

func CreateRelayFixedCost(c *gin.Context) {
	var input relayFixedCostInput
	if relayRefundDecode(c, &input) != nil || input.Estimated == nil {
		relayRiskError(c, model.ErrRelayInput)
		return
	}
	if !relayFinanceProof(c, "fixed_cost", input) {
		return
	}
	err := model.RecordRelayFixedCost(model.DB, model.RelayFixedCost{ID: input.ID, Month: input.Month, Category: input.Category, AmountFen: input.AmountFen,
		Estimated: *input.Estimated, Supersedes: input.Supersedes, Reason: input.Reason, ReviewerID: c.GetInt("id")})
	if err != nil {
		relayRiskError(c, err)
		return
	}
	relayPayJSON(c, 200, gin.H{"recorded": true}, "")
}

type relayRiskEvaluateHandler struct{}

func (relayRiskEvaluateHandler) Type() string { return model.SystemTaskTypeRelayRiskEvaluate }
func (relayRiskEvaluateHandler) Enabled() bool {
	return common.GetEnvOrDefaultBool("RELAY_WALLET_SCHEMA", false) && common.GetEnvOrDefaultBool("RELAY_RISK_CONTROL", false)
}
func (relayRiskEvaluateHandler) Interval() time.Duration { return time.Minute }
func (relayRiskEvaluateHandler) NewPayload() any         { return nil }
func (relayRiskEvaluateHandler) Run(ctx context.Context, task *model.SystemTask, runnerID string) {
	if !(relayRiskEvaluateHandler{}).Enabled() {
		finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusFailed, nil, errors.New("risk control disabled"))
		return
	}
	if err := ctx.Err(); err != nil {
		finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusFailed, nil, err)
		return
	}
	r, err := model.EvaluateRelayRisk(model.DB.WithContext(ctx), time.Now())
	status := model.SystemTaskStatusSucceeded
	if err != nil {
		status = model.SystemTaskStatusFailed
	}
	if err == nil && (r.State.Mode == "warning" || r.State.Mode == "stopped") {
		common.SysLog("Relay financial risk: " + r.State.Mode + "; reason=" + r.State.Reason)
	}
	finishSystemTaskHandler(task, runnerID, status, map[string]any{"mode": r.State.Mode, "revision": r.State.Revision}, err)
}

var _ service.ScheduledSystemTaskHandler = relayRiskEvaluateHandler{}
