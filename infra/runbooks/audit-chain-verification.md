# Runbook: Audit Chain Verification

**Owner:** Security Operations
**Alert routes:** `https://runbooks.trade.internal/audit-chain-verification`
**Authority:** 22_AUDIT_INTEGRITY_AND_EVIDENCE.md; 06_SECURITY_AND_ACCESS_CONTROL.md §6.

---

## 0. Verification cadence

| Check | Cadence | Source |
|---|---|---|
| Incremental audit verification | **hourly** | 22 §3: "Audit verification runs hourly and after restore/deployment" |
| Full audit verification | **daily** | 22 §3: "full verification runs daily" |
| Verification after restore or deployment | **every occurrence** | 22 §3 |
| Audit chain verification after database/regional recovery | **mandatory before reopening** | 22 §7 |
| Audit restore and key recovery test | **quarterly** | 22 §4 |
| Signed audit checkpoint | **daily**, and on key rotation or high-impact security event | 23 §6 |

---

## 1. Understand what you are verifying

1. Audit evidence must be append-only, attributable, tamper-evident, independently retained, and verifiable **without trusting the application database alone** (22 §1).
2. Records are ordered by a monotonic sequence within a partition.
3. `record_hash = SHA-256(canonical_json(record_without_record_hash_and_signature))`; `previous_hash` references the prior record hash in that partition (22 §3).
4. Canonical serialization and schema version are fixed and tested. A serialization change invalidates historical hashes by definition.

## 2. Run incremental verification

5. Verify each partition's hash chain from the last verified checkpoint forward.
6. Check for:
   - **missing sequence** — a gap in the monotonic sequence;
   - **invalid hash** — a record whose recomputed hash does not match;
   - **signature failure** — a checkpoint or record whose signature does not verify;
   - **unexpected checkpoint** — a checkpoint outside the expected cadence or range;
   - **retention gaps** — evidence missing from the independent immutable store.
7. Verify the latest signed checkpoint covers the expected sequence range, first/last hash, count, timestamp, and signing key ID (22 §3).

## 3. Run full verification

8. Recompute the entire chain from the genesis record to the latest checkpoint.
9. Verify every partition independently.
10. Verify that the immutable cross-region copy contains every record the operational store claims to hold.
11. Verify the application runtime identity has **write-only append** permissions and **cannot delete or shorten retention** (22 §4). If it can, this is a finding independent of the chain result.

## 4. Interpret the result

12. **Any chain break, missing sequence, signature failure, or unexpected checkpoint is a SEV-1 security event** (22 §3).
13. On failure:
    - **stop privileged mutations and risk-increasing activity**;
    - **preserve evidence**;
    - **alert the owner and security operator**.
14. A financial mutation is blocked if the required audit commit failed (23 §4: "financial mutation is blocked if required audit commit fails").
15. Do **not** repair, re-sign, or backfill the chain. 24_ENTERPRISE_RELEASE_STANDARD.md §16 prohibits "silent audit deletion or mutation". A repair attempt destroys the evidence of the break.
16. Deletion is permitted only after retention expiry, legal-hold clearance, and two-person approval — and the deletion event itself is retained (22 §4).

## 5. Verify after restore or deployment

17. After a database or regional recovery, verify chain continuity **from the last signed checkpoint** (22 §7).
18. Compare audit sequence ranges against the transactional outbox and the immutable object copies.
19. Resolve every gap **before** reopening privileged or risk-increasing operations (22 §7).
20. After a deployment, verify the chain again. A deployment that breaks the chain is a release-blocking defect, not an operational nuisance.

## 6. Verify independent retention

21. Confirm at least one independent copy is cross-region and protected by **separate administrative credentials** (22 §4).
22. Confirm the retention is seven years for financial, access, approval, and control records (22 §4; 05 §Data retention).
23. Confirm legal hold overrides ordinary deletion and that the legal hold is itself audited (18 §3).
24. Quarterly: test key recovery and audit restore (22 §4).

## 7. Verify redaction and access

25. Confirm no secret, authentication factor, raw token, or unnecessary personal data appears in any audit payload (22 §6; 06 §6). Sensitive values are represented by stable references or keyed digests, not copies.
26. Confirm audit views enforce the same resource scopes as the source systems (22 §6).
27. Confirm confidential payloads are referenced by immutable object ID and digest with separately controlled access.
28. Confirm audit export is purpose-bound, time-limited, and logged (22 §6).

## 8. Record

29. Record the verification result: partitions checked, sequence ranges, checkpoint identifiers, signing key IDs, result, operator, UTC timestamp.
30. On failure, record it as a SEV-1 with corrective actions, owners, and expiry.
31. Any key rotation involved triggers a signed audit checkpoint (23 §6).

## 9. Acceptance tests that must exist

32. Per 22 §7, the acceptance suite must demonstrate: **tamper detection, deletion denial, signature verification, duplicate delivery idempotency, checkpoint recovery, key rotation, cross-region restore, and evidence query by correlation ID**.
33. A verification run that does not exercise all eight does not satisfy the acceptance criterion.
