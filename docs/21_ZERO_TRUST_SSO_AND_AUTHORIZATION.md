# Zero Trust, SSO and Authorization Specification

## 1. Decision

Use Keycloak as the reference centralized identity broker for workforce identity and OIDC as the application protocol. Support SAML 2.0 only as an upstream federation option. All application services consume normalized identity claims and perform local policy enforcement. Open Policy Agent (OPA) is the reference authorization policy evaluator for subject/resource/context policy; the Go Risk Engine remains the sole financial-risk authority. The identity provider authenticates; the platform authorization service decides whether a subject may perform a specific operation. Authentication is never treated as authorization.

## 2. Human authentication protocol

1. Browser is redirected to the broker using OIDC Authorization Code + PKCE.
2. Broker enforces phishing-resistant MFA for privileged roles and risk-based step-up for sensitive actions.
3. Callback validates issuer, audience, signature, nonce, state, exact redirect URI, and token lifetime.
4. Backend creates an opaque server-side session; browser receives only a Secure, HttpOnly, SameSite cookie.
5. Session references the subject, auth strength, auth time, role/permission revision, and session expiry.
6. Every privileged mutation rechecks session validity, current permission revision, action scope, and required auth freshness.
7. Logout, account disablement, or role revocation invalidates server sessions and refresh tokens within 60 seconds.

Access tokens are not stored in browser local storage. Refresh tokens are rotated, replay-detected, and revoked on reuse. OIDC scopes are minimal. Production local-password fallback is prohibited.

## 3. Session and step-up policy

| Session class | Idle timeout | Absolute timeout | Step-up freshness |
|---|---:|---:|---:|
| Privileged operator | 15 minutes | 8 hours | 5 minutes for high-impact actions |
| Read-only operator/auditor | 30 minutes | 8 hours | 15 minutes for confidential export |
| Service workload | 15-minute credential TTL target | 1 hour maximum token lifetime | Not applicable; workload identity and mTLS required |

If the identity broker or authorization service is unavailable, deny new sessions, permission changes, live activation, risk-increasing commands, and credential operations. Already accepted non-financial read-only requests may continue only if their session remains valid and the local policy cache is within its 5-minute maximum age.

## 4. Permission decision model

Authorization evaluates the following tuple:

`subject_id + subject_type + action + resource_type + resource_id + environment + market_scope + account_scope + auth_strength + policy_version + request_context`.

Rules: deny by default; explicit deny overrides allow; permissions are narrowly scoped; policy evaluation is server-side; frontend visibility is not a security control; service identities cannot impersonate humans; and authorization decisions are auditable. Resource ownership does not imply permission. No wildcard grants are allowed for live financial operations.

## 5. Role baseline

| Role | Allowed baseline | Explicitly excluded |
|---|---|---|
| OWNER | Manage system scope, request activation, approve eligible high-impact changes | Bypass of risk, eligibility, dual-control, or gate checks |
| TRADING_OPERATOR | Observe and operate approved strategies within assigned scope; request pause/cancel | Change risk policy or approve own live promotion |
| RISK_OPERATOR | Review risk policy, halt, investigate, approve re-enable with second approver | Submit orders or approve own policy change |
| RESEARCHER | Create experiments, backtests, model/strategy candidates | Live credentials, direct OMS access, self-approval |
| PLATFORM_OPERATOR | Deploy approved artifacts, operate infrastructure, restore services | Change trading risk limits or authorize live trading |
| AUDITOR | Read approved evidence and audit views | Mutations, secret access, trading actions |
| SERVICE | Execute only assigned workload contract | Human login, cross-environment access, arbitrary tool invocation |

Every role is further constrained by market, account, environment, and resource scope. Role grants require request, second-person approval, expiry, and review. Privileged grants expire after 90 days unless re-approved.

## 6. Dual control and approval workflow

High-impact changes create immutable approval requests with proposed diff, scope, reason, policy version, requester, and expiry. A distinct approver reviews the exact diff and current risk/eligibility status. Any change to the diff invalidates approval. Approval expires after 24 hours and must be renewed. Live activation requires fresh step-up authentication, all gates G0–G10 PASS, eligibility current, no active critical incident, a successful reconciliation snapshot, and explicit owner confirmation.

## 7. Service identity and delegation

Use workload identity federation and short-lived audience-bound tokens. Each worker has a unique service principal, environment, allowed API audience, and explicit capability list. Delegation is attenuated: a child agent cannot receive permissions broader than its parent and cannot obtain financial capability unless its workload class is explicitly approved. Research agents are read-only against trading control APIs. Human-to-service delegation records the initiating actor and expires with the task.

## 8. Break-glass

Break-glass uses two-person release, incident reference, 30-minute maximum duration, and immediate security alert. It grants infrastructure recovery access only; it never bypasses financial authorization, Risk Engine, OMS, reconciliation, jurisdiction eligibility, or live gates. All actions are captured in the audit integrity system and reviewed within one business day.

## 9. Required verification

Automated acceptance tests must prove: unauthenticated request denied; invalid issuer/audience/nonce denied; expired/revoked session denied; role removed takes effect within 60 seconds; cross-market/account access denied; stale step-up denied; self-approval denied; changed approval diff invalidates approval; service token audience mismatch denied; research identity cannot reach live endpoints; identity outage fails closed for risk-increasing actions; and break-glass cannot bypass financial controls.


## 10. Policy bundle lifecycle

Authorization policies are version-controlled, peer-reviewed, signed, and deployed as immutable bundles. Every decision records policy bundle digest and decision result. Production policy changes require test vectors for allow, deny, explicit-deny precedence, scope isolation, stale identity, and emergency revocation. If OPA or its bundle is unavailable/stale beyond five minutes, deny new sessions, privileged mutations, live activation, and risk-increasing actions; only valid read-only access may continue under the bounded cache rule in Section 3. Trading-risk rules are compiled and evaluated by the Go Risk Engine and cannot be delegated to OPA or an AI model.
