# Security and Access Control

## 1. Security architecture and trust boundaries

Adopt Zero Trust: no request is trusted because it originates inside a network, from a known device, or from an AI agent. Every human and workload request is authenticated, authorized for the exact action and resource, evaluated against environment and session context, and logged. Network location is a defense layer, never an authorization grant.

Zones are separated into edge, application, data, management, and recovery. Public ingress terminates at managed WAF/load balancing. Databases, event transport, secret stores, signing services, and venue egress remain private. Egress is deny-by-default and allowlisted by workload, destination, protocol, and environment. Production administration uses a dedicated access path with device posture checks; ordinary application nodes cannot administer infrastructure.

## 2. SSO and identity lifecycle

The identity broker is the sole human authentication authority. The web application uses OpenID Connect Authorization Code flow with PKCE, exact redirect URI allowlists, `state` and `nonce` validation, issuer/audience validation, short-lived sessions, secure HttpOnly SameSite cookies, and server-side session revocation. SAML 2.0 federation is supported only at the broker boundary for enterprise identity providers; applications consume normalized OIDC identity claims and do not implement separate SAML logic. Local password authentication is disabled for production.

Require phishing-resistant MFA (FIDO2/WebAuthn passkeys or hardware security keys) for all privileged users. Step-up authentication is required for live activation, risk-policy changes, credential rotation, permission grants, account/venue enablement, emergency override, and export of confidential records. Re-authentication freshness is at most 5 minutes for live activation and other high-impact actions. Sessions idle-expire after 15 minutes for privileged operators and 30 minutes for read-only users; absolute lifetime is 8 hours. Revocation must invalidate server-side sessions within 60 seconds.

SCIM 2.0 is the supported provisioning/deprovisioning interface where the identity provider supports it. Otherwise, owner-managed provisioning follows the same approval and audit controls. Disablement must revoke sessions and workload delegation within 60 seconds. Joiner/mover/leaver events are audited. Shared human accounts are prohibited.

## 3. Authorization model

Use RBAC for baseline job functions and ABAC constraints for resource, environment, market, account, strategy, action, time, and authentication strength. Every permission is evaluated server-side on every request. Default is deny. Wildcard permissions are prohibited for financial actions. Permissions are represented as explicit tuples: `subject`, `action`, `resource`, `environment`, `market_scope`, `account_scope`, `conditions`, `approval_requirement`.

Roles: `OWNER`, `TRADING_OPERATOR`, `RISK_OPERATOR`, `RESEARCHER`, `PLATFORM_OPERATOR`, `AUDITOR`, and `SERVICE`. Role membership alone never grants live activation or unrestricted account access. Granting/revoking privileged roles requires a second authorized approver; the requester cannot approve their own request. `OWNER` is still subject to strong authentication, scope checks, immutable audit, and live activation gates.

Financial control actions requiring dual control: live-mode activation, venue/account enablement, increasing risk limits, changing loss/drawdown controls, disabling a halt, approving a model/strategy for live promotion, and rotating live venue credentials. The initiating actor and approver must be distinct identities. Emergency halt is always available to authorized halt operators and does not require a second approver; re-enable does.

## 4. Workload identity and service-to-service security

Every service and worker has a unique workload identity bound to its deployment and environment. Use short-lived workload credentials and mutual TLS for private service-to-service calls. Audience-restricted tokens identify the intended service; token exchange is permitted only through the identity broker. No static shared service secrets, user tokens in queues, or credentials embedded in container images. Python research workers have no live environment identity. Venue adapters receive only venue-specific, account-specific, least-privilege credentials.

## 5. Break-glass and privileged access

Break-glass access is sealed, time-limited, monitored, and reserved for identity-provider or access-plane failure. It requires two-person release, incident ID, reason, and maximum 30-minute session. It cannot bypass Risk Engine, OMS, ledger, or live activation gates. Every use triggers immediate alerting and post-incident review within one business day. Infrastructure privilege is JIT and expires automatically.

## 6. Secrets, cryptographic keys, and data protection

Secrets reside in a managed secret store. Use separate key hierarchies per environment and purpose; production keys are non-exportable where supported. Encrypt transport with TLS 1.2 minimum (TLS 1.3 preferred) and storage with managed envelope encryption. Rotate service credentials at most every 90 days or immediately on suspected exposure; rotate signing keys at least annually and after compromise, with overlapping verification keys during planned rotation. Live venue credentials are scoped to required API functions; withdrawal/transfer permissions are prohibited unless separately justified and approved, and are disabled by default.

Secrets, authentication factors, private keys, and raw access tokens never enter source, logs, prompts, analytics, test fixtures, or client bundles. Redact sensitive fields at ingestion, not only at dashboard display. Data export requires purpose, scope, expiry, approval where confidential/restricted, and an audit record.

## 7. Application and supply-chain controls

Mandatory controls: schema validation; CSRF protection for cookie-authenticated mutations; secure headers; content security policy; request/body limits; rate limits; replay/idempotency protection; dependency pinning; SAST; dependency and license scanning; DAST on release candidates; container/IaC scanning; SBOM; signed build provenance; protected branches; mandatory code review; secret scanning; artifact digest verification; and audited production deployment. Critical vulnerability remediation target is 24 hours for exploitable internet-facing components and 7 days for other production components; exceptions require compensating controls, owner, expiry, and risk approval.

## 8. Threat handling and security acceptance

Threats include credential theft, session fixation, unauthorized order submission, privilege escalation, AI prompt/tool injection, malicious research artifacts, supply-chain compromise, market-data poisoning, replay/duplicate submission, secret leakage, destructive configuration, and recovery-region compromise. Mitigations include separate authorities, scoped identities, deterministic risk veto, signed artifacts, content-addressed research outputs, immutable audit, egress controls, idempotency, and independent reconciliation.

Security acceptance requires verified SSO/MFA, role and ABAC negative tests, session revocation test, workload identity tests, secret scanning, threat-model review, external penetration test before live activation and annually thereafter, and closure or formally accepted remediation for every critical/high finding. A failed identity or authorization dependency denies risk-increasing actions.
