package controller

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// The API stays closed until a coordinated rollout; the worker has its own gate.
func RelayRefundAPIGate(c *gin.Context) {
	if !common.GetEnvOrDefaultBool("RELAY_WALLET_SCHEMA", false) || !common.GetEnvOrDefaultBool("RELAY_REFUND_API", false) {
		relayPayJSON(c, 503, nil, "refunds unavailable")
		c.Abort()
		return
	}
	c.Next()
}

// Explicit projection excludes merchant identifiers, raw evidence and private
// reviewer notes. Do not serialize the database model to a user response.
func relayRefundView(r model.RelayRefund, admin bool) gin.H {
	view := gin.H{"id": r.ID, "order_id": r.OrderID, "amount_fen": r.AmountFen, "state": r.State,
		"frozen": r.Frozen, "created_at": r.CreatedAt, "updated_at": r.UpdatedAt, "needs_review": r.NeedsReview}
	if admin {
		view["user_id"] = r.UserID
		view["reviewer_id"] = r.ReviewerID
		view["reason"] = r.Reason
	}
	return view
}

func relayRefundError(c *gin.Context, err error) {
	status, message := 500, "refund operation unavailable"
	switch {
	case errors.Is(err, model.ErrRelayInput):
		status, message = 400, "invalid refund request"
	case errors.Is(err, gorm.ErrRecordNotFound):
		status, message = 404, "refund or order not found"
	case errors.Is(err, model.ErrRelayFunding):
		status, message = 409, "funding reconciliation required"
	case errors.Is(err, model.ErrRelayConflict), errors.Is(err, model.ErrRelayQuota), errors.Is(err, model.ErrRelayState):
		status, message = 409, "refund conflicts with current balance or state"
	}
	relayPayJSON(c, status, nil, message)
}

func relayRefundDecode(c *gin.Context, value any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 2048))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return model.ErrRelayInput
	}
	return nil
}

func CreateRelayRefund(c *gin.Context) {
	var body struct {
		ID        string `json:"id"`
		OrderID   string `json:"order_id"`
		AmountFen int64  `json:"amount_fen"`
	}
	if relayRefundDecode(c, &body) != nil {
		relayRefundError(c, model.ErrRelayInput)
		return
	}
	r, err := model.RequestRelayRefund(model.DB, c.GetInt("id"), body.ID, body.OrderID, body.AmountFen)
	if err != nil {
		relayRefundError(c, err)
		return
	}
	relayPayJSON(c, 200, relayRefundView(r, false), "")
}

func GetRelayRefund(c *gin.Context) {
	r, err := model.GetRelayRefund(model.DB, c.GetInt("id"), c.Param("id"))
	if err != nil {
		relayRefundError(c, err)
		return
	}
	relayPayJSON(c, 200, relayRefundView(r, false), "")
}

func relayRefundList(c *gin.Context, admin bool) {
	page, _ := strconv.Atoi(c.DefaultQuery("p", "1"))
	userID := c.GetInt("id")
	if admin {
		userID = 0
	} else if userID <= 0 {
		relayRefundError(c, model.ErrRelayInput)
		return
	}
	items, total, err := model.ListRelayRefunds(model.DB, userID, page)
	if err != nil {
		relayRefundError(c, err)
		return
	}
	views := []gin.H{}
	for _, r := range items {
		views = append(views, relayRefundView(r, admin))
	}
	relayPayJSON(c, 200, gin.H{"items": views, "total": total, "page": page, "page_size": 20}, "")
}
func ListRelayRefunds(c *gin.Context) { relayRefundList(c, false) }

// Defence in depth: a claimed role in request data is never administrator authority.
func relayRefundAdmin(c *gin.Context) bool {
	var admin model.User
	if model.DB.Where("id = ? AND role >= ? AND status = ?", c.GetInt("id"), common.RoleAdminUser, common.UserStatusEnabled).Take(&admin).Error != nil {
		relayPayJSON(c, 403, nil, "administrator required")
		return false
	}
	return true
}
func ListAdminRelayRefunds(c *gin.Context) {
	if relayRefundAdmin(c) {
		relayRefundList(c, true)
	}
}

func ReviewRelayRefund(c *gin.Context) {
	if !relayRefundAdmin(c) {
		return
	}
	var body struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if relayRefundDecode(c, &body) != nil || (body.Decision != "approve" && body.Decision != "reject") || strings.TrimSpace(body.Reason) == "" {
		relayRefundError(c, model.ErrRelayInput)
		return
	}
	operation := struct {
		ID       string `json:"id"`
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}{c.Param("id"), body.Decision, body.Reason}
	if !relayFinanceProof(c, "refund_review", operation) {
		return
	}
	err := model.ReviewRelayRefund(model.DB, c.Param("id"), c.GetInt("id"), strings.TrimSpace(body.Reason), body.Decision == "approve")
	if err != nil {
		relayRefundError(c, err)
		return
	}
	relayPayJSON(c, 200, gin.H{"reviewed": true}, "")
}

func GetRelayRefundBalance(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("p", "1"))
	balance, items, total, err := model.GetRelayRefundBalance(model.DB, c.GetInt("id"), page)
	if err != nil {
		relayRefundError(c, err)
		return
	}
	relayPayJSON(c, 200, gin.H{"balance": balance, "items": items, "total": total, "page": page, "page_size": 20}, "")
}

func InitializeEmptyRelayFunding(c *gin.Context) {
	if err := model.InitializeEmptyRelayFunding(model.DB, c.GetInt("id")); err != nil {
		relayRefundError(c, err)
		return
	}
	relayPayJSON(c, 200, gin.H{"ready": true}, "")
}
