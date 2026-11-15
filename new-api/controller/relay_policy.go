package controller

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// Administrator-only publication of price versions and the global generation
// limit. Both switches default to off; reads never expose anything to users.
func RelayPolicyAPIGate(c *gin.Context) {
	if !common.GetEnvOrDefaultBool("RELAY_WALLET_SCHEMA", false) || !common.GetEnvOrDefaultBool("RELAY_POLICY_API", false) || !service.RelayPolicyEnabled() {
		relayPayJSON(c, 503, nil, "price and limit publication unavailable")
		c.Abort()
		return
	}
	if !relayRefundAdmin(c) {
		c.Abort()
		return
	}
	c.Next()
}

func relayPolicyError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrRelayPolicyDisabled):
		relayPayJSON(c, 503, nil, "price and limit publication unavailable")
	case errors.Is(err, model.ErrRelayInput):
		relayPayJSON(c, 400, nil, "invalid publication")
	case errors.Is(err, model.ErrRelayConflict), errors.Is(err, model.ErrRelayState):
		relayPayJSON(c, 409, nil, "publication conflicts with the current version")
	default:
		common.SysError(common.LogText("relay policy operation failed: %s", err.Error()))
		relayPayJSON(c, 500, nil, "publication failed")
	}
}

func relayPolicyDecode(c *gin.Context, value any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 8192))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return model.ErrRelayInput
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return model.ErrRelayInput
	}
	return nil
}

func GetRelayPolicy(c *gin.Context) {
	if len(c.Request.URL.Query()) != 0 {
		relayPolicyError(c, model.ErrRelayInput)
		return
	}
	view, err := service.ReadRelayPolicy()
	if err != nil {
		relayPolicyError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	relayPayJSON(c, 200, view, "")
}

func PublishRelayPolicyPrice(c *gin.Context) {
	var input service.RelayPricePublication
	if relayPolicyDecode(c, &input) != nil {
		relayPolicyError(c, model.ErrRelayInput)
		return
	}
	if !relayFinanceProof(c, "price_publish", input) {
		return
	}
	row, err := service.PublishRelayPrice(c.GetInt("id"), input)
	if err != nil {
		relayPolicyError(c, err)
		return
	}
	relayPayJSON(c, 200, row, "")
}

func PublishRelayPolicyLimits(c *gin.Context) {
	var input service.RelayLimitsPublication
	if relayPolicyDecode(c, &input) != nil {
		relayPolicyError(c, model.ErrRelayInput)
		return
	}
	if !relayFinanceProof(c, "limits_publish", input) {
		return
	}
	row, err := service.PublishRelayLimits(c.GetInt("id"), input)
	if err != nil {
		relayPolicyError(c, err)
		return
	}
	relayPayJSON(c, 200, row, "")
}
