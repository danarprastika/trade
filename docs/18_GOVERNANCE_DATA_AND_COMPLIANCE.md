# Governance, Data Handling and Compliance Boundary

## 1. Accountability and separation of duties

The owner is accountable for business purpose, eligible account holder status, jurisdiction scope, market enablement, risk mandate, and live activation. Engineering owns system integrity and release evidence. Risk operations owns policy review, halt decisions, and re-enable validation. Research owns experiment quality and lineage but cannot self-approve live promotion. Platform operations owns deployment and recovery. Auditors receive read-only evidence access. Enforce separation in authorization; a process document alone is insufficient.

## 2. Decision and audit records

Record identity and authorization changes, configuration revisions, risk evaluations, strategy/model approvals, command acceptance/rejection, order transitions, venue acknowledgements, reconciliation cases, halts/activation, deployment, data exports, policy decisions, and privileged access. Each record includes actor/workload identity, action, target, environment, UTC timestamp, reason, correlation/causation IDs, policy version, and before/after references where applicable. Integrity and independent retention requirements are defined in `21_AUDIT_INTEGRITY_AND_EVIDENCE.md`.

## 3. Data classification and lifecycle

- Public: published market information and public documentation.
- Internal: system metrics and non-sensitive operational metadata.
- Confidential: strategy logic, model artifacts, account identifiers, non-public positions, and trading records.
- Restricted: credentials, signing/recovery secrets, authentication factors, and regulated personal data.

Apply least privilege, encryption, retention, and export controls by classification. Restricted data is excluded from logs, prompts, client bundles, and general analytics. Personal data collection is purpose-limited and minimized. Access, correction, deletion, legal hold, and export workflows must be supported where applicable law requires them. Retention schedules are configurable by data class and legal hold; legal hold overrides ordinary deletion and is itself audited.

## 4. AI governance boundary

AI outputs are untrusted proposals. Tool permissions are explicit and environment-scoped. Retrieved content cannot grant authority. Material AI-assisted decisions retain model/version, prompt/template version, tool invocation IDs, evidence references, and outcome. Credentials and unnecessary personal data are excluded from model context. AI cannot approve its own artifacts, alter risk policy, clear halts, grant access, or activate live execution.

## 5. Jurisdiction and venue eligibility policy

The platform uses a positive eligibility registry. A country, market class, instrument, venue, account, and activity are disabled until the required review is complete; API availability is not evidence of legal permission. No default jurisdiction is inferred from IP address, device location, language, or account currency. The owner must declare the account holder's legal residence and contracting entity; the platform records the evidence source and review date.

Before enabling a scope, complete a signed eligibility record covering: (1) legal capacity and account-holder eligibility, including applicable age requirements; (2) jurisdiction and entity/account status; (3) venue authorization and terms; (4) permitted market/instrument and product type; (5) API access and automation restrictions; (6) data licensing and redistribution; (7) tax/accounting record obligations; (8) sanctions/restricted-party screening where applicable; (9) privacy/data residency obligations; (10) reporting/record-retention requirements; and (11) required advice from qualified local counsel or compliance professionals. The review identifies the responsible reviewer, sources, scope, effective date, expiry/review date, and restrictions.

A legal/compliance review must determine the applicable rules for each intended jurisdiction; this blueprint does not assert that any specific jurisdiction or venue is approved. The registry is versioned and machine-enforced. Unknown, expired, contradictory, or revoked eligibility records fail closed. Scope expansion requires a new approval. The platform blocks live activation when the account holder is not legally eligible, required adult/guardian arrangements are not valid under applicable law, or the evidence cannot be established. No live credentials are provisioned before eligibility approval.

Market access is segmented by asset class and instrument. Equity, derivatives, leveraged products, margin, shorting, and cross-border access are disabled by default and require separate explicit eligibility and risk approvals. No policy may override a legal or venue restriction.

### 5.1 Initial jurisdiction profile and rule-change control

The initial compliance review profile is Indonesia. It applies only when the account holder explicitly declares Indonesia as the relevant residence/jurisdiction; the system must not infer this from device location, IP, locale, or payment currency. This profile is a review scope, not a representation that the platform, account, venue, product, or activity is legally approved.

For Indonesian digital financial assets and crypto assets, the rule registry must account for OJK supervision and the current applicable instruments, including POJK 27/2024 as amended by POJK 23/2025, and PADK OJK 3/2026 effective 1 September 2026. The system must consult current OJK official provider/instrument information at onboarding and before activation; no static provider list is embedded as a permanent approval. These instruments and official lists can change, so the compliance owner maintains a versioned regulatory register with publication date, effective date, source URL, affected product/activity, reviewer, decision, and next review date.

For Indonesia crypto scope, the platform requires all of the following before a live scope can be enabled: (1) account holder eligibility and identity status; (2) current OJK provider authorization status for the exact provider and service; (3) current instrument eligibility/listing status; (4) venue API and automation permission; (5) applicable product-specific conditions, including any knowledge test, margin, or derivatives restrictions; (6) consumer/account terms; (7) data and recordkeeping obligations; and (8) a dated compliance approval. A provider or asset appearing on a list does not itself establish that a particular user, API use, strategy, or product is permitted.

For equities, FX, commodities, derivatives, margin, shorting, and cross-border access, the same eligibility registry applies independently. Each product-market pair requires a documented determination of regulator, licensed intermediary/venue, account permissions, product classification, automation restrictions, reporting, tax-record requirements, and data rights. If the applicable rule set cannot be established, the relevant market remains disabled. No blanket permission is inferred from an API being technically reachable.

Regulatory monitoring runs daily against configured official sources and generates a review case on publication, amendment, withdrawal, provider-list change, or source unavailability. An eligibility record is revalidated at least every 30 days and immediately on a relevant regulatory/provider change. If a source cannot be refreshed within 72 hours, the affected scope is blocked from new risk-increasing activity until refreshed; existing positions remain observable and must follow the approved safe-management procedure. Compliance owners may shorten these intervals, never lengthen them without a recorded risk approval.

The platform must refuse live activation for a person who is not legally eligible to hold and operate the relevant account, including applicable minimum-age requirements. It must not support account sharing, false declarations, or bypass of identity/age checks. If an applicable law provides a special arrangement, it is not enabled unless counsel documents its applicability and the product explicitly supports the required controls.

## 6. Vendor, dependency, and data-provider governance

Regulatory source monitoring maintains a versioned register of authority, instrument title/number, official URL, publication date, effective date, superseded provisions, affected product/activity scopes, reviewer, interpretation record, and next review. Daily monitoring detects new or amended official publications; future-effective rules create scheduled change cases and are assessed before their effective date. Source text is evidence, not an automated legal interpretation. A qualified reviewer must map the change to account/product/activity scope and approve the resulting policy version. If a source is unavailable beyond the configured tolerance, the affected scope is blocked from new risk-increasing activity.

For Indonesia digital financial assets/crypto, official references include OJK POJK 23/2025 amending POJK 27/2024 (effective 10 November 2025) and PADK OJK 3/2026 (effective 1 September 2026). The current official publications are maintained at: [OJK POJK 23/2025](https://ojk.go.id/id/regulasi/Pages/POJK-23-2025-Perubahan-POJK-27-Tahun-2024-tentang-Penyelenggaraan-Perdagangan-Aset-Keuangan-Digital-Termasuk-Aset-Kripto.aspx) and [OJK PADK 3/2026](https://ojk.go.id/id/regulasi/Pages/PADK-Nomor-3-Tahun-2026-Penyelenggaraan-Perdagangan-Aset-Keuangan-Digital-termasuk-Aset-Kripto.aspx). This reference list is not exhaustive and does not establish eligibility for a provider, account, product, or person.

Every production provider has an owner, purpose, data categories, access scope, failure mode, availability dependency, terms/data-rights review, security review, and exit plan. Critical providers have an alternate or explicit safe-degradation behavior. Dependency changes are pinned, scanned, reviewed, and included in SBOM/provenance. Unmaintained or critical-vulnerability dependencies cannot be promoted without remediation or an approved, time-bounded compensating control.

## 7. Governance acceptance

A production scope is accepted only when responsibility is assigned, data classification and retention are recorded, provider dependencies are reviewed, audit evidence is queryable and integrity-checked, access is least-privilege, jurisdiction/venue eligibility is current, and required approvals are recorded. Live activation requires all applicable evidence and owner authorization. This section defines engineering controls, not legal, tax, or investment advice.
