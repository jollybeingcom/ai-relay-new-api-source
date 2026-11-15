# Changes from upstream New API

Modified by the AI Relay project. Relevant modification date: 2026-11-12.
This source snapshot was prepared on 2026-11-15.

Based on upstream commit `7d8b91268dab69d7150f345c448b5e250163146a`. Changes provide durable wallet
reservations and settlement, immutable price/cost snapshots, per-user and global
admission, idempotent Alipay/GoPay orders, refunds and funding audits, risk controls,
audited adjustments, and versioned price/concurrency policies. Real payments
remain disabled without merchant configuration; simulated checks are not real
payment acceptance. Existing native UI attribution is preserved.

`Dockerfile` contains the tested immutable runtime layer, download cache/retry
steps and verified Go proxy fallback. Complete modified files are already applied
in `new-api/`; no private overlay tools or repository are required to build.

The following files differ from upstream or are added. Their exact bytes and
all other source files are recorded in `SOURCE-MANIFEST.json`.

- `Dockerfile`
- `controller/relay_adjustment.go`
- `controller/relay_adjustment_test.go`
- `controller/relay_anomaly.go`
- `controller/relay_funding_review.go`
- `controller/relay_pay.go`
- `controller/relay_pay_test.go`
- `controller/relay_policy.go`
- `controller/relay_policy_test.go`
- `controller/relay_refund.go`
- `controller/relay_refund_api.go`
- `controller/relay_refund_api_test.go`
- `controller/relay_refund_test.go`
- `controller/relay_risk.go`
- `controller/relay_risk_test.go`
- `controller/relay_token.go`
- `controller/relay_usage.go`
- `controller/relay_usage_test.go`
- `controller/relay_wallet_chat_test.go`
- `controller/relay_wallet_maintenance.go`
- `controller/relay_wallet_maintenance_test.go`
- `controller/relay_wallet_public.go`
- `controller/system_task_handlers.go`
- `controller/user_quota.go`
- `go.mod`
- `go.sum`
- `middleware/access_token_routes.go`
- `middleware/auth.go`
- `middleware/relay_legacy_funding.go`
- `middleware/relay_legacy_funding_test.go`
- `model/main.go`
- `model/relay_adjustment.go`
- `model/relay_adjustment_test.go`
- `model/relay_anomaly.go`
- `model/relay_funding.go`
- `model/relay_funding_review.go`
- `model/relay_funding_test.go`
- `model/relay_pay.go`
- `model/relay_pay_test.go`
- `model/relay_policy.go`
- `model/relay_policy_test.go`
- `model/relay_refund.go`
- `model/relay_refund_test.go`
- `model/relay_refund_view.go`
- `model/relay_risk.go`
- `model/relay_risk_test.go`
- `model/relay_token.go`
- `model/relay_usage.go`
- `model/relay_wallet.go`
- `model/relay_wallet_admission_test.go`
- `model/relay_wallet_cost.go`
- `model/relay_wallet_cost_test.go`
- `model/relay_wallet_holds.go`
- `model/relay_wallet_receipt.go`
- `model/relay_wallet_test.go`
- `model/system_task.go`
- `relay/channel/api_request.go`
- `relay/channel/api_request_redirect_test.go`
- `relay/compatible_handler.go`
- `relay/request_billing.go`
- `router/api-router.go`
- `router/relay_legacy_funding_test.go`
- `router/relay_refund_scope_test.go`
- `service/access_token_scope.go`
- `service/billing.go`
- `service/relay_finance_verification.go`
- `service/relay_finance_verification_test.go`
- `service/relay_pay.go`
- `service/relay_pay_test.go`
- `service/relay_policy.go`
- `service/relay_policy_test.go`
- `service/relay_refund.go`
- `service/relay_refund_test.go`
- `service/relay_refund_worker.go`
- `service/relay_wallet_chat.go`
- `service/relay_wallet_chat_test.go`
- `service/relay_wallet_price.go`
- `service/relay_wallet_session.go`
- `service/relay_wallet_session_test.go`
- `service/security_verification.go`
- `service/tiered_settle.go`
- `service/violation_fee.go`
