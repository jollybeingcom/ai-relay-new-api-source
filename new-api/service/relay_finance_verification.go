package service

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"

	"github.com/QuantumNous/new-api/common"
)

const VerificationScopeRelayFinance = "relay.finance.write"

type RelayFinanceContext struct {
	Action string `json:"action"`
	Digest string `json:"digest"`
}

var relayFinanceDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

func validRelayFinanceContext(context RelayFinanceContext) bool {
	if !relayFinanceDigest.MatchString(context.Digest) {
		return false
	}
	switch context.Action {
	case "risk_control", "fixed_cost", "adjustment", "funding_review", "refund_review", "price_publish", "limits_publish":
		return true
	}
	return false
}

// Bind the exact validated operation body, including reason and expected
// revision, into the upstream single-use session/PAT security proof.
func RelayFinanceOperation(action string, body any) (VerificationOperation, error) {
	raw, err := common.Marshal(body)
	if err != nil {
		return VerificationOperation{}, err
	}
	digest := sha256.Sum256(raw)
	context := RelayFinanceContext{Action: action, Digest: hex.EncodeToString(digest[:])}
	if !validRelayFinanceContext(context) {
		return VerificationOperation{}, ErrVerificationContextInvalid
	}
	encoded, err := common.Marshal(context)
	return VerificationOperation{Scope: VerificationScopeRelayFinance, Context: encoded}, err
}
