package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/require"
)

func TestRelayFinanceVerification(t *testing.T) {
	for _, state := range []model.UserVerificationState{{Role: common.RoleAdminUser, HasPassword: true}, {Role: common.RoleCommonUser, HasTwoFA: true}} {
		_, err := securityVerificationPolicy(VerificationScopeRelayFinance, state)
		require.ErrorIs(t, err, ErrVerificationForbidden)
	}
	for _, state := range []model.UserVerificationState{{Role: common.RoleAdminUser, HasTwoFA: true}, {Role: common.RoleAdminUser, HasPasskey: true}} {
		methods, err := securityVerificationPolicy(VerificationScopeRelayFinance, state)
		require.NoError(t, err)
		require.Len(t, methods, 1)
		require.NotEqual(t, VerificationMethodPassword, methods[0].Method)
	}
	scope, exists := AccessTokenVerificationScope(VerificationScopeRelayFinance)
	require.True(t, exists)
	require.Equal(t, "billing:write", scope)
	op, err := RelayFinanceOperation("fixed_cost", map[string]any{"id": "one", "amount_fen": 100})
	require.NoError(t, err)
	binding, err := BindVerificationOperation(op)
	require.NoError(t, err)
	other, err := RelayFinanceOperation("fixed_cost", map[string]any{"id": "one", "amount_fen": 101})
	require.NoError(t, err)
	changed, err := BindVerificationOperation(other)
	require.NoError(t, err)
	require.NotEqual(t, binding.ContextHash, changed.ContextHash)
	for _, context := range []string{`{}`, `{"action":"unknown","digest":"` + strings.Repeat("a", 64) + `"}`, `{"action":"fixed_cost","digest":"bad"}`, `{"action":"fixed_cost","digest":"` + strings.Repeat("a", 64) + `","extra":1}`} {
		_, err := BindVerificationOperation(VerificationOperation{Scope: VerificationScopeRelayFinance, Context: json.RawMessage(context)})
		require.Error(t, err)
	}
}
