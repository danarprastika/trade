# Runbook: Regional Failover into RECOVERY_HOLD

**Owner:** Platform Operations (execution) — Incident Commander + Risk Owner (authorization)
**Alert routes:** `https://runbooks.trade.internal/regional-failover-recovery-hold`
**Authority:** 10_OPERATIONS_AND_DISASTER_RECOVERY.md §Recovery topology and failover authority; 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §2; 25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_PROFILE.md §6.

> **Recovery never automatically resumes live operations.** 23 §4: "Recovery never
> automatically resumes live operations." 10: "cross-region replicas are not promoted
> automatically." This runbook ends in `RECOVERY_HOLD`, not in normal operation.

---

## 1. Authorize failover

1. Failover requires **all** of the following (10 §Recovery topology):
   - Incident Commander approval;
   - Risk Owner approval;
   - a recorded incident;
   - confirmation of database consistency;
   - a documented venue-state reconciliation plan.
2. Record both approver identities. They MUST be distinct (06_SECURITY_AND_ACCESS_CONTROL.md §3).
3. If either approver is unavailable, failover does not proceed automatically. Use break-glass per 06 §5 (two-person release, 30-minute maximum) and record the incident.

## 2. Confirm the trigger is real

4. Confirm the primary region is genuinely unavailable or unrecoverable within the objective.
5. Recovery objectives: **RTO ≤ 30 minutes, RPO ≤ 5 minutes** (08_NFR_OBSERVABILITY_AND_CAPACITY.md §Recovery objectives; 10 §Recovery topology; 12_DECISION_REGISTER.md ADR-025).
6. These are engineering targets demonstrated by quarterly drills. They are not a licence to fail over early: failing over is itself a financial event.

## 3. Enter RECOVERY_HOLD

7. Enter `RECOVERY_HOLD` **before** any promotion. 10: "After recovery, the system starts in RECOVERY_HOLD: read-only observation and reconciliation are allowed; new risk-increasing commands, strategy deployment, credential rotation, and live activation remain blocked."
8. Confirm the platform is in `RECOVERY_HOLD` and record the entry timestamp.
9. Confirm the blocked capabilities are actually blocked. A `RECOVERY_HOLD` that permits a risk-increasing command is a defect.

## 4. Promote the recovery region

10. Topology is **active-passive**. There is one authoritative primary region at a time; there are no active-active financial writes (23 §2).
11. Promote the recovery-region standby. Do not create a second writable financial authority.
12. Restore from the latest valid backup and replay WAL to the selected recovery point (10 §Restore procedure).
13. Verify the recovery point is within the RPO of 5 minutes. Record the actual data-loss assessment.

## 5. Validate schema and ledger invariants

14. Validate the schema is at the expected version. Migrations must be compatible with the immediately previous release (24 §5).
15. Validate ledger invariants: entries are append-only and balanced; corrections are compensating entries, never destructive updates (05 §Ledger).
16. Verify the audit chain continuity from the last signed checkpoint (22_AUDIT_INTEGRITY_AND_EVIDENCE.md §7: "After database or regional recovery, verify chain continuity from the last signed checkpoint, compare audit sequence ranges with transactional outbox and immutable object copies, and resolve gaps before reopening privileged or risk-increasing operations").
17. Resolve any audit gap before continuing. A gap is not a documentation issue; it is evidence loss.

## 6. Run reconciliation against venue records

18. Run reconciliation against available venue records (10 §Restore procedure).
19. Reconcile orders, fills, balances, and positions.
20. Orders whose venue state cannot be established remain `UNKNOWN`. **The system must not retry an exposure-increasing command** (10 §Recovery topology).

## 7. Run smoke tests

21. Run smoke and fault checks.
22. Verify: Risk Engine authoritative; OMS consistent; market-data freshness within limits; risk configuration valid and verified; signing keys verified.

## 8. Obtain re-enable authorization

23. Re-enable requires **all** of the following (10 §Recovery topology):
   - database/ledger invariants pass;
   - order/fill/balance/position reconciliation completed **or explicitly dispositioned**;
   - market-data freshness verified;
   - risk configuration and signing keys verified;
   - smoke/fault checks pass;
   - **two distinct authorized approvers sign the scope-specific release**.
24. Record the recovery evidence: recovery point, elapsed time, data-loss assessment, reconciliation result, approvals, artifact/config digests, and follow-up actions (10 §Recovery topology).
25. 01_SYSTEM_ARCHITECTURE.md §10 invariant 4: "No restored environment may leave RECOVERY_HOLD without independent verification." The two approvers ARE that independent verification.

## 9. Controlled resume

26. Re-open controlled operations only after step 8 is complete. 10 §Restore procedure: "and only then reopen controlled operations."
27. Resume in the **narrowest scope**. Resume read-only observation first; resume reconciliation next; resume risk-increasing activity last.
28. Live activation remains a separate decision requiring G11. Passing recovery does not satisfy G11.

## 10. Failover back to the original region

29. Failback is a second failover and follows this runbook from step 1.
30. Do not fail back while a `RECOVERY_HOLD` condition is unresolved.
31. Verify the original region is genuinely healthy before failing back; a premature failback creates a second incident.

## Prohibited

- Automatic promotion of a cross-region replica.
- Automatic resumption of risk-increasing activity after failover.
- Failing over without a recorded incident and two distinct approvers.
- Re-enabling while an `UNKNOWN` order exists.
- Creating a second authoritative financial writer.
