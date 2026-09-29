# Audit Integrity, Evidence and Independent Retention

## 1. Objective and design choice

Audit evidence must be append-only, attributable, tamper-evident, independently retained, and verifiable without trusting the application database alone. Use a signed hash-chain ledger plus immutable object retention and independent verification. Do not introduce a public blockchain or token solely for audit: it adds operational and privacy complexity without improving the platform's trust model.

## 2. Event record schema

Every audit record contains: `audit_id`, `sequence`, `tenant_or_owner_scope`, `actor_id`, `actor_type`, `action`, `target_type`, `target_id`, `environment`, `market_scope`, `occurred_at_utc`, `recorded_at_utc`, `reason`, `correlation_id`, `causation_id`, `policy_version`, `result`, `before_digest`, `after_digest`, `previous_hash`, `record_hash`, and `signing_key_id`. Sensitive values are represented by stable references or keyed digests, not copied into audit payloads.

## 3. Hash chain and signing

Records are ordered by a monotonic sequence within a partition. `record_hash = SHA-256(canonical_json(record_without_record_hash_and_signature))`; `previous_hash` references the prior record hash in that partition. Every batch is closed with a signed checkpoint containing partition, sequence range, first/last hash, count, timestamp, and signing key ID. Checkpoints are signed by a managed KMS/HSM key. Canonical serialization and schema version are fixed and tested.

A chain break, missing sequence, signature failure, or unexpected checkpoint is a SEV-1 security event. The system stops privileged mutations and risk-increasing activity, preserves evidence, and alerts the owner/security operator. Audit verification runs hourly and after restore/deployment; full verification runs daily.

## 4. Independent storage and retention

The authoritative operational audit stream is written transactionally with the business mutation or outbox event. A separate audit exporter copies signed batches to a distinct account/project and region using object-lock/WORM retention. Application runtime identities have write-only append permissions and cannot delete or shorten retention. Security/audit readers have read-only access. Retention is seven years for financial, access, approval, and control records unless applicable law or an approved legal hold requires longer. Routine operational logs follow the shorter retention policy in the NFR document and are not substitutes for audit records.

At least one independent copy is cross-region and protected by separate administrative credentials. Key recovery and audit restore are tested quarterly. Deletion is permitted only after retention expiry, legal-hold clearance, and two-person approval; the deletion event itself is retained.

## 5. Evidence lineage

A release evidence bundle links source commit, build workflow, artifact digest, SBOM, provenance attestation, test results, security findings, migration result, deployment identity, configuration fingerprint, gate approvals, and rollback artifact. A strategy/model decision links dataset fingerprint, feature definition version, code revision, model artifact digest, evaluation report, reviewer, approval, and deployment scope. A live order links initiating actor/agent, strategy version, risk-policy version, risk decision, command idempotency key, OMS transitions, venue request/response references, fills, reconciliation result, and ledger postings.

## 6. Access, privacy, and redaction

Audit views enforce the same resource scopes as source systems. Secrets, authentication factors, raw tokens, and unnecessary personal data are never recorded. Confidential payloads are referenced by immutable object ID and digest with separately controlled access. Audit export is purpose-bound, time-limited, and logged.

## 7. Recovery and acceptance

After database or regional recovery, verify chain continuity from the last signed checkpoint, compare audit sequence ranges with transactional outbox and immutable object copies, and resolve gaps before reopening privileged or risk-increasing operations. Acceptance tests must demonstrate tamper detection, deletion denial, signature verification, duplicate delivery idempotency, checkpoint recovery, key rotation, cross-region restore, and evidence query by correlation ID.
