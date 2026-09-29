# API and Integration Contracts

## 1. API boundary

The Go Control Plane is the sole authoritative application API. Browser and Telegram clients authenticate to it; research workers use service identities and a separate service audience. Clients never connect directly to PostgreSQL, the event broker, or venue APIs.

All endpoints use `/api/v1`. OpenAPI is the normative HTTP contract. JSON request and response bodies use UTF-8. Unknown request fields are rejected on commands and tolerated only on explicitly designated read projections. All mutation requests require `Idempotency-Key`, `X-Correlation-ID`, authenticated actor identity, and environment scope. The server generates identifiers when omitted only for non-idempotent read workflows; financial commands must provide or receive a durable command identity before processing.

## 2. Resource groups

- `GET /health/live`, `GET /health/ready`: liveness and readiness only; no secrets or internal topology.
- `/api/v1/session`, `/api/v1/identities`, `/api/v1/roles`: identity and access administration.
- `/api/v1/markets`, `/api/v1/instruments`, `/api/v1/market-data`: normalized market metadata and feed status.
- `/api/v1/strategies`, `/api/v1/research-runs`, `/api/v1/model-artifacts`: research artifacts and lifecycle commands.
- `/api/v1/risk/policies`, `/api/v1/risk/evaluations`: policy configuration and explainable evaluations.
- `/api/v1/orders`, `/api/v1/orders/{order_id}/cancel`: canonical OMS commands and state views.
- `/api/v1/positions`, `/api/v1/balances`, `/api/v1/ledger`, `/api/v1/reconciliation-cases`: authoritative or explicitly labeled projections.
- `/api/v1/halts`, `/api/v1/activations`, `/api/v1/audit-events`: privileged operational controls and evidence.

A route may be implemented only with an owning domain, authorization policy, request/response schema, idempotency behavior, audit requirement, failure semantics, and contract tests. The route groups above are the complete public API surface for the initial platform; new groups require an ADR and contract update.

## 3. Command response semantics

A command response distinguishes `accepted`, `rejected`, and `outcome_unknown`. `accepted` means the platform durably accepted the command; it does not mean an external venue filled it. Venue submission state is reported by OMS events and queries. `outcome_unknown` is returned when a timeout prevents proving whether a downstream side effect occurred. Clients must query the canonical command/order rather than blindly retrying with a new key.

Use HTTP 200/201 for completed reads or resource creation, 202 for durably accepted asynchronous commands, 400 for malformed input, 401 for unauthenticated requests, 403 for unauthorized requests, 409 for state/idempotency conflicts, 422 for domain validation or risk rejection, 429 for rate limiting, 503 for unavailable dependencies, and 504 only when no durable command was accepted. Error bodies use the canonical error taxonomy and include `error_code`, `message`, `correlation_id`, and safe structured details.

## 4. Authentication and authorization

Human sessions use secure, HttpOnly, SameSite cookies and phishing-resistant MFA. Service-to-service calls use short-lived workload credentials with audience, environment, and scope claims. Every request is authorized server-side against actor, resource, environment, operation, and current halt/activation state. UI visibility is not an authorization control.

## 5. Request limits, pagination, and abuse controls

Maximum request body is 1 MiB; command payloads are limited to 64 KiB unless a route-specific schema sets a lower bound. Read collections use cursor pagination: default page size 50, maximum 200, opaque server-generated cursor, stable sort key, and explicit continuation token. Offset pagination is prohibited for mutable financial collections. Rate limits are enforced by identity, environment, account, and route class. Default operator limits are 60 read requests/minute and 10 privileged mutation requests/minute; financial command throughput is additionally constrained by the account risk policy and venue-specific limits. The server returns `429` with a retry hint, never silently drops or queues a command outside the documented command semantics. WAF limits and application limits are both required.

## 6. Integration rules

Synchronous internal calls use HTTPS/JSON by default. Protobuf/gRPC is permitted only for measured high-volume internal paths and must preserve the same canonical schemas and authorization context. Events are versioned and documented in AsyncAPI-compatible form. Venue adapters implement a common capability contract and may not bypass OMS, risk, ledger, or reconciliation.

## 7. Contract acceptance

A contract change is accepted only when OpenAPI/event schemas validate, compatibility tests pass, idempotency and authorization tests pass, errors are documented, and the change has an owning service and migration/rollback plan. Breaking changes require a new major API version or an explicitly coordinated migration; silent semantic changes are prohibited.
