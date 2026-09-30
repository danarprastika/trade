# Runbook: Quarterly Recovery Exercise

**Owner:** Platform Operations (execution) — Incident Commander + Risk Owner (authorization)
**Alert routes:** `https://runbooks.trade.internal/quarterly-recovery-exercise`
**Authority:** 10_OPERATIONS_AND_DISASTER_RECOVERY.md §Drill schedule; 08_NFR_OBSERVABILITY_AND_CAPACITY.md §Recovery objectives; 25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_PROFILE.md §5 Resilience.

> **Cadence:** 10 §Drill schedule — "Quarterly regional recovery exercise." The
> `QuarterlyRecoveryExerciseOverdue` alert fires at 90 days. 12_DECISION_REGISTER.md
> ADR-025: recovery objectives must not be **claimed** until drills demonstrate them.

---

## 1. Schedule and scope

1. Schedule the exercise at least 30 days before the quarter closes.
2. Define the exercise scope in advance and record it: which failure is injected, which region, which environment, and the abort criteria.
3. Announce the exercise. An exercise is not a production event, but it must not be so disruptive that it is indistinguishable from one.

## 2. Establish the baseline

4. Record the pre-exercise state: artifact digests, configuration fingerprint, feature-flag state, ledger root hash, and the last signed audit checkpoint.
5. Confirm `RECOVERY_HOLD` entry criteria are understood by all participants. 10: "After recovery, the system starts in RECOVERY_HOLD: read-only observation and reconciliation are allowed; new risk-increasing commands, strategy deployment, credential rotation, and live activation remain blocked."

## 3. Inject the failure

6. Inject the scenario. 24_ENTERPRISE_RELEASE_STANDARD.md §13 requires the enterprise test program to exercise regional failover among others.
7. For a regional exercise, simulate loss of the primary region. **Never** perform the exercise against the live production financial path with real venue credentials. Use paper/shadow or an isolated recovery project.
8. Record the declared incident start time. This is the RTO measurement origin (08 §Recovery objectives: "Recovery objectives are measured from declared incident start to restored service").

## 4. Execute failover into RECOVERY_HOLD

9. Follow `regional-failover-recovery-hold` in full. Do not shortcut steps; the exercise exists to test the procedure as written.
10. Confirm the platform entered `RECOVERY_HOLD` and that the blocked capabilities were actually blocked.

## 5. Execute the restore

11. Follow `backup-restore-verification` steps 6-9: restore the latest valid backup, replay WAL to the selected recovery point, validate schema and ledger invariants.
12. Verify audit chain continuity from the last signed checkpoint, and compare audit sequence ranges against the transactional outbox and the immutable cross-region object copies (22 §7).
13. Run reconciliation against available venue records.
14. Run smoke and fault checks.

## 6. Measure against objectives

15. Record and measure:
    - **RTO**: elapsed time from declared incident start to restored service. Target **≤ 30 minutes** for the critical control plane.
    - **RPO**: data-loss window. Target **≤ 5 minutes** for the critical control plane.
16. Record the actual observed values. Do not round, estimate, or report the target as the measurement.
17. If either target is missed, that is a FAIL result for the exercise and requires documented remediation plus a retest. 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §6: a shortfall "cannot be waived by documentation".

## 7. Verify safety invariants held

18. Confirm that throughout the exercise:
    - no risk-increasing command was accepted while authoritative state was unavailable (01_SYSTEM_ARCHITECTURE.md §6);
    - no order was retried while in `UNKNOWN` state (04 §Order lifecycle);
    - no exposure-increasing retry occurred;
    - the halt was not silently cleared;
    - no `UNKNOWN` state was converted to a deterministic outcome by assumption;
    - audit evidence was never silently dropped.

    A failure of any of these is a more serious finding than a missed RTO.

## 8. Test re-enable authorization

19. Attempt re-enable **without** the two distinct approvers and confirm it is **denied**. The negative test matters as much as the positive one.
20. Then perform the re-enable with two distinct approvers and confirm it succeeds and is audited.

## 9. Record and close

21. Record the full exercise report (24 §13: "Each scenario must record the expected safety state, observed state, evidence digest, duration, operator, and corrective action"):
    - expected safety state;
    - observed state;
    - evidence digest;
    - duration;
    - operators;
    - corrective actions with owners and expiry.
22. Store the report immutably.
23. Update the `recovery_last_quarterly_exercise_timestamp_seconds` signal so the alert clears.
24. File the audit checkpoint (23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §6: daily and on key rotation / high-impact security event).

## Companion drills (10 §Drill schedule)

| Drill | Cadence | Runbook |
|---|---|---|
| Backup restore verification | monthly (05 §Backup: weekly) | `backup-restore-verification` |
| Regional recovery exercise | quarterly | this runbook |
| Kill-switch exercise | quarterly | `kill-switch-halt-activation` |
| Full incident simulation (venue outage, database outage, credential compromise) | semiannual | `incident-sev1-response` |
| Audit chain verification | hourly; full daily | `audit-chain-verification` |
| Key recovery and audit restore | quarterly | 22_AUDIT_INTEGRITY_AND_EVIDENCE.md §4 |
