# Configuration and Risk Policy Standard

## 1. Configuration authority

Configuration is typed, schema-validated, versioned, environment-scoped, and immutable after activation. A change creates a new configuration revision with actor, reason, timestamp, approval, diff, and effective scope. Runtime services consume a validated snapshot; they do not read arbitrary mutable key/value settings during a financial decision.

Secrets are referenced by secret-store identifiers, never embedded in configuration documents. Configuration promotion follows dev, test, staging, paper, shadow, and live. Live configuration cannot be copied automatically from lower environments.

## 2. Fail-safe defaults

A new account, strategy, instrument, venue, or environment starts disabled for risk-increasing actions. Missing limits, unknown precision, missing market rules, unavailable account state, stale inputs, unapproved strategy/model, unresolved material reconciliation case, or active halt means reject. No default value may silently imply permission to trade.

## 3. Required policy dimensions

A risk policy must explicitly define, for its account/strategy/instrument/environment scope:

- permitted markets, venues, instruments, order types, and direction;
- maximum order notional and quantity;
- maximum gross/net exposure, leverage, concentration, and open orders;
- loss/drawdown thresholds and the measurement window/source;
- market-data freshness limits and allowed price-deviation bounds;
- order-rate and cancel-rate limits;
- fee, funding, margin, and settlement assumptions where applicable;
- permitted operating mode and schedule;
- escalation, halt, and re-enable authority.

Each numeric limit must specify currency or unit, aggregation scope, measurement window, boundary inclusivity, source of truth, and behavior on missing data. The platform supplies no universal live numeric trading limits: the owner must configure values appropriate to the account, venue, jurisdiction, and risk mandate. Until the complete policy is validated and approved, live activation is blocked. This is a closed fail-safe rule, not an unresolved design choice.

## 4. Evaluation semantics

Risk evaluation is deterministic for a fixed policy revision, account state, market snapshot, and command. The result contains `approved` or `rejected`, policy revision, evaluated facts, failed controls, timestamp, and correlation ID. A risk approval is short-lived and bound to the exact command, instrument, quantity, price constraints, account, environment, and policy revision. Any material change invalidates approval and requires reevaluation.

Risk checks run immediately before the OMS permits submission. Risk approval is not a guarantee of venue acceptance, fill, or profit. Risk state uncertainty fails closed for new risk. Risk-reducing actions may proceed only through a separately defined, audited safe-reduction policy and must not increase net exposure.

## 5. Halt controls

Halt activation is immediately effective and monotonic in severity. Lower-level commands cannot clear higher-level halts. Clearing requires the owning authority, a recorded reason, health checks, reconciliation status, and a two-person approval for system-wide live re-enable. Emergency halt remains available independently of AI services and noncritical UI components.

## 6. Policy acceptance

A policy revision is accepted only after schema validation, boundary tests, scenario tests, authorization review, audit verification, and a dry-run comparison against representative account states. Production activation requires an authorized approver and a rollback revision. No model or strategy may modify its own risk policy or grant itself permission.
