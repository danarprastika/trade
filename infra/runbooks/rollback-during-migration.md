# Runbook: Rollback During Database Migration

**Owner:** Platform Operations
**Alert routes:** `https://runbooks.trade.internal/rollback-during-migration`
**Authority:** 24_ENTERPRISE_RELEASE_STANDARD.md §5 (migration compatibility) and §15 (rollback restores previous artifact AND configuration); 09_TESTING_AND_RELEASE_EVIDENCE.md §Release strategy; 11_EXECUTION_GATES.md §G1/G2.

> **This runbook covers a failure discovered after a migration is already applied.**
> It does not replace the requirement that migrations be backward-compatible with the
> immediately previous release. If rollback is only possible by destroying data,
> the migration violated the compatibility requirement — that is the finding.

---

## 0. Preconditions that must hold before deploying

1. The migration is backward-compatible with the immediately previous release
   (24 §5). No drop/rename in the same release that stops reading it.
2. The previous release remains deployable while the migration is in flight.
3. The rollback plan is defined before deployment, not invented during the incident.
4. The migration is a tracked, reviewed, authorized change. 24 §13: "Database changes require a recorded rollback plan and evidence."
5. If the migration touched a ledger table, the rollback must not require a
   destructive update. Ledger corrections are compensating entries only (05 §Ledger).

---

## 1. Declare the rollback

6. Roll back when any of these is true:
   - the new release causes a correctness or safety failure;
   - the migration causes query performance or error-rate regression beyond the alert threshold (08 §4: control plane >2%/5m, risk/OMS >0.5%/5m);
   - a ledger invariant or reconciliation break is caused by the schema change;
   - the audit chain verification fails after deployment (22 §7);
   - rollback is safer than a forward fix within the incident window.
7. Record the incident and the rollback decision. If the change was an emergency
   change, it already requires retrospective review within one business day
   (10 §Change management); a rollback does not cancel that obligation.
8. If the failure affects authoritative state, risk, or order correctness, treat
   the rollback as a SEV-1 and activate the halt first per
   `kill-switch-halt-activation` before changing the schema.

## 2. Capture the pre-rollback state

9. Record before changing anything:
   - current artifact digest;
   - current configuration fingerprint;
   - current migration version;
   - ledger root hash and record count;
   - last signed audit checkpoint identifier;
   - active feature-flag state.
10. This snapshot is the evidence that the rollback actually restored the
    pre-deployment state. 24 §15: rollback "restores the previous artifact and
    configuration snapshot".

## 3. Apply the application rollback

11. Redeploy the previous immutable artifact by digest. Do not rebuild it.
12. Restore the previous configuration snapshot.
13. Restore the previous feature-flag state. A feature flag left in the post-release
    state can silently re-enable incompatible behaviour with the older artifact.
14. Confirm the running artifact and configuration digests match step 9's snapshot.

## 4. Handle the schema

15. **Preferred:** the old release works against the new schema because the
    migration was backward-compatible. Verify this explicitly; do not assume it.
16. If the old release does not work against the new schema, the migration violated
    the compatibility requirement. Escalate as a release-process defect.
17. Apply only the down-migration that was reviewed and authorized as part of the
    original change. Do not author new down-migrations during an incident.
18. For ledger tables: **never** run a destructive down-migration. The correct action
    is a compensating entry recorded through the normal application path.

## 5. Verify

19. Verify the pre-rollback state from step 9 is restored: artifact digest, config
    fingerprint, migration version, feature flags.
20. Run smoke tests.
21. Re-run the full audit chain verification (22 §7). A rollback is a deployment;
    verification runs after it.
22. Verify ledger invariants and run reconciliation.
23. Verify no order was silently lost or duplicated by the rollback. Any order in
    `UNKNOWN` state stays `UNKNOWN` (04 §Order lifecycle).

## 6. Data and event integrity

24. The event bus uses at-least-once delivery with idempotent consumers
    (05 §Event delivery). After a rollback, consumers from two release versions may
    both have processed events. Verify idempotency held.
25. The transactional outbox is the source of truth for event publication
    (05 §Outbox). Verify committed outbox records were not lost or duplicated.
26. Durable consumers resume from their checkpointed position and remain
    deterministic (05 §Event delivery). Verify the consumer checkpoints are
    consistent with the restored state.

## 7. Close

27. Record the rollback as a release event: the failed release digest, the restored
    digest, the configuration snapshots, the reason, the data-loss assessment, and
    the reconciliations performed.
28. The failed release does not count toward a successful promotion. The release
    must be re-validated from its own gate before any retry (11 §G1/G2).
29. Open a corrective action to fix the migration's compatibility so that the
    rollback path is genuinely non-destructive next time.

## Prohibited

- Reverting code without reverting configuration, or vice versa.
- Running an unauthorized or newly authored down-migration during the incident.
- Destructive down-migration of a ledger table.
- Marking the failed release as rolled-back-and-fine when ledger or audit
  verification did not pass.
- Resuming live operations from the rollback without the normal re-enable
  authorization; a rollback is a recovery, and recovery starts in `RECOVERY_HOLD`
  (10 §Recovery topology).
