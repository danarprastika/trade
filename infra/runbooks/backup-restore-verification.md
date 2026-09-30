# Runbook: Backup Restore Verification

**Owner:** Platform Operations
**Alert routes:** `https://runbooks.trade.internal/backup-restore-verification`
**Authority:** 05_PERSISTENCE_EVENTING_RECONCILIATION.md §Backup; 10_OPERATIONS_AND_DISASTER_RECOVERY.md §Restore procedure and §Drill schedule.

> **Cadence:** 05 §Backup requires **weekly** restore verification. 10 §Drill schedule
> requires **monthly** backup restore verification. The stricter control (weekly)
> governs; the monthly cadence is the minimum. The `BackupRestoreVerificationOverdue`
> alert fires at 30 days.

---

## 1. Scope and prerequisites

1. Select the environment to verify. Run this in a **non-live** environment or a
   dedicated restore-verification project. Never restore over the live primary.
2. Confirm the backup catalogue:
   - continuous WAL archiving is active;
   - a daily full backup exists for the target date;
   - the target backup is within the **35 daily restore points** or **12 monthly
     restore points** schedule (05 §Backup);
   - backups are encrypted and access-controlled (05 §Backup).
3. Confirm the restore target is in a separately secured region. 22_AUDIT_INTEGRITY_AND_EVIDENCE.md §4 requires at least one independent cross-region copy.

## 2. Select the recovery point

4. Choose a recovery point from the most recent valid backup plus WAL replay.
5. Record the recovery point precisely. This is the RPO measurement input.

## 3. Restore

6. Restore the latest valid backup (10 §Restore procedure).
7. Replay WAL to the selected recovery point (10 §Restore procedure).
8. Record elapsed time from incident start to restored service. This is the RTO measurement input.

## 4. Validate schema and ledger invariants

9. Validate the schema version matches the expected release.
10. Validate ledger invariants:
    - every entry references its source command/event and correlation identifier (05 §Ledger);
    - entries are append-only and balanced;
    - corrections are compensating entries, never destructive updates;
    - no duplicate application of a compensating entry.
11. Verify the audit chain is continuous from the last signed checkpoint (22 §7).

## 5. Run reconciliation

12. Run reconciliation against available venue records (10 §Restore procedure).
13. Classify every difference with severity, owner, evidence, and aging SLA.
14. A material unresolved break means the restore **did not succeed** for the affected scope. Repeat from step 6 against an earlier recovery point and open an incident.

## 6. Run smoke tests

15. Run smoke tests (10 §Restore procedure).
16. Verify: Risk Engine authoritative; OMS state machine consistent; ledger queryable; market data within freshness limits; risk configuration loads and validates.

## 7. Record evidence

17. Record the verification evidence (10 §Recovery topology: "Recovery evidence includes recovery point, elapsed time, data-loss assessment, reconciliation result, approvals, artifact/config digests, and follow-up actions"):
    - recovery point;
    - elapsed time (RTO measurement);
    - data-loss assessment (RPO measurement);
    - reconciliation result;
    - approvals;
    - artifact and configuration digests;
    - corrective actions.
18. Store the evidence immutably with a content digest (22 §4: audit evidence uses object-lock/WORM retention).
19. Destroy the verification environment after recording. Do not promote it to service.

## 8. Assess against objectives

20. Compare the measured values against the objectives:
    - **RTO ≤ 30 minutes** (critical control plane);
    - **RPO ≤ 5 minutes** (critical control plane);
    - research services RTO ≤ 4 hours, RPO ≤ 24 hours (08 §Recovery objectives).
21. If either target is missed, open a corrective action. 12_DECISION_REGISTER.md ADR-025: "The platform must not claim these objectives until restore/failover drills demonstrate them." A missed target is a finding, not a documentation note.

## 9. Close

22. Confirm the restore-verification timestamp is recorded so the `BackupRestoreVerificationOverdue` alert clears.
23. File the audit checkpoint if a key rotation or high-impact security event occurred during the exercise (23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §6).
