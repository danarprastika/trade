# Runbook: Key Rotation

**Owner:** Security Operations (execution) — Platform Operations (support)
**Alert routes:** `https://runbooks.trade.internal/key-rotation`
**Authority:** 06_SECURITY_AND_ACCESS_CONTROL.md §6; 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §6; 21_ZERO_TRUST_SSO_AND_AUTHORIZATION.md §5.

---

## 0. Rotation schedule

| Key class | Maximum interval | Source |
|---|---|---|
| Service credentials | **90 days**, or immediately on suspected exposure | 06 §6: "Rotate service credentials at most every 90 days or immediately on suspected exposure" |
| Signing keys | **annually**, and after compromise | 06 §6: "rotate signing keys at least annually and after compromise, with overlapping verification keys during planned rotation" |
| Privileged role grants | **90 days** | 21 §5: "Privileged grants expire after 90 days unless re-approved" |
| Venue credentials (live) | on permission change, venue API change, or exposure | 16_MARKET_DATA_AND_VENUE_ADAPTERS.md §6: "Certification expires after material adapter, venue API, permission, or contract changes" |

> Rotation uses **overlapping verification keys** during planned rotation (06 §6).
> An emergency rotation is different: revoke first, verify later. Do not leave a
> compromised key valid "for a transition period".

---

## 1. Classify the rotation

1. **Planned** — periodic rotation on schedule. Use the overlap procedure in step 3.
2. **Emergency** — suspected exposure, compromise, or a security event. Go to step 2 immediately; skip the overlap.
3. **Live venue credential** — additionally requires dual control. 06 §3 lists "rotating live venue credentials" as a financial control action requiring dual control.
4. **Signing key** — additionally triggers a signed audit checkpoint. 23 §6: "Audit checkpoint | Daily and on key rotation / high-impact security event."

## 2. Emergency rotation — revoke first

5. **Revoke the exposed key immediately.** Do not wait for a replacement to be issued. 25 §6: "Secret/key suspected compromised | Revoke/rotate through controlled procedure, invalidate dependent sessions, assess signed audit and artifact integrity."
6. Invalidate dependent sessions. Session revocation must propagate within **60 seconds** (23 §6; 21 §2).
7. Revoke workload delegation for the affected identity (06 §2: "Disablement must revoke sessions and workload delegation within 60 seconds").
8. Enter restricted mode if compromise of credentials, signing keys, or audit infrastructure is suspected (24_ENTERPRISE_RELEASE_STANDARD.md §9).
9. Preserve forensic evidence before replacing anything. Do not delete the evidence that the compromise produced.
10. Assess the integrity of signed audit records and release artifacts. A compromised signing key means prior signatures cannot be trusted without re-verification against an independent record.
11. If live credentials were exposed, page SECURITY immediately — this is a SEV-1 (`LiveCredentialExposureDetected`).

## 3. Planned rotation — overlapping verification

12. Generate the new key in the KMS/HSM. Keys are **non-exportable where supported** (06 §6).
13. Publish the new verification key alongside the current one. Both remain valid during the overlap window.
14. Roll consumers to accept signatures from BOTH keys.
15. Wait for the overlap window to elapse. Do not shorten it to save time; a shortened overlap converts a planned rotation into an outage.
16. Retire the old key.
17. Confirm all consumers verify with the new key before retiring the old one.

## 4. Rotate environment-isolated keys

18. **Rotate per environment, never across environments.** 01_SYSTEM_ARCHITECTURE.md §4: each environment has separate encryption keys. 06 §6: "Use separate key hierarchies per environment and purpose."
19. Rotating a lower-environment key MUST NOT touch the live key hierarchy, and rotating the live key MUST NOT be observable from a lower environment.
20. Verify isolation after rotation: confirm a lower environment cannot read the live key material. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §2: "Live secrets are not readable by lower environments."

## 5. Rotate the audit signing key

21. The audit signing key requires additional care (22_AUDIT_INTEGRITY_AND_EVIDENCE.md §3):
    - records are signed by a managed KMS/HSM key;
    - every batch is closed with a signed checkpoint containing partition, sequence range, first/last hash, count, timestamp, and signing key ID.
22. Use overlapping verification keys so historical checkpoints remain verifiable.
23. **Do not re-sign history.** Prior checkpoints stay verifiable under the old key. Re-signing would destroy the evidence of the rotation.
24. Run a full audit chain verification after the rotation (22 §3: full verification runs daily, and after restore/deployment).

## 6. Rotate the artifact signing key

25. Update the CI signing configuration to the new key version.
26. Re-verify provenance for artifacts signed with the previous key. 06 §6 requires "overlapping verification keys during planned rotation".
27. Confirm the protected registry still rejects overwrites (immutable tags). 23 §2 requires immutable OCI images.
28. Record the rotation in the release evidence.

## 7. Rotate privileged human access

29. Privileged grants expire after **90 days** unless re-approved (21 §5).
30. Granting or revoking privileged roles requires a second authorized approver; the requester cannot approve their own request (06 §3).
31. Verify revocation propagates: a role removed must take effect within **60 seconds** (21 §2, §9).

## 8. Verify

32. Confirm the new key is in use everywhere the old key was.
33. Confirm the old key is fully retired and produces verification failures.
34. Run the full audit chain verification.
35. Confirm no service is silently failing signature verification and logging errors instead of failing closed.

## 9. Record

36. Record the rotation: key identifiers, key versions, environments affected, operators, UTC timestamps, and the audit checkpoint reference.
37. Key rotation is a high-impact security event and requires a signed audit checkpoint (23 §6).
38. Retain the rotation record under the 7-year audit retention (05 §Data retention; 22 §4).

## 10. After a compromise

39. Perform recovery from trusted artifacts (24 §9).
40. Recovery enters `RECOVERY_HOLD`; it does not resume live operations automatically.
41. Re-enable requires two distinct authorized approvers and the full re-enable checklist (10 §Recovery topology).
