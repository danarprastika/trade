# Operations and Disaster Recovery

## Incident command

SEV-1 incidents require an Incident Commander, Technical Lead, Risk Owner, and Communications Owner. The Incident Commander controls coordination; the Risk Owner controls trading halt/re-enable decisions.

## Immediate response to uncontrolled behavior

1. Activate system halt.
2. Block new risk-increasing commands.
3. Preserve logs, audit records, and deployment state.
4. Establish whether venue state is known.
5. Reconcile orders, fills, balances, and positions.
6. Restore authoritative state.
7. Validate risk and data health.
8. Re-enable in the narrowest affected scope.
9. Produce incident evidence and corrective actions.

## Disaster recovery

Primary and recovery environments are provisioned from Terraform. PostgreSQL WAL and backups are replicated to the recovery location. Recovery is executed using immutable application artifacts and versioned configuration.

## Restore procedure

Restore the latest valid backup, replay WAL to the selected recovery point, validate schema and ledger invariants, run reconciliation against available venue records, run smoke tests, and only then reopen controlled operations.

## Drill schedule

Monthly backup restore verification. Quarterly regional recovery exercise. Quarterly kill-switch exercise. Semiannual full incident simulation covering venue outage, database outage, and credential compromise.

## Change management

All production changes require a ticket, code review, automated evidence, deployment plan, rollback plan, and authorized release. Emergency changes require retrospective review within one business day.


## Recovery objectives and failover authority

- Critical control-plane recovery time objective: 30 minutes; recovery point objective: 5 minutes. These are engineering targets and must be demonstrated by quarterly regional recovery exercises.
- Recovery topology is active-passive. Financial writes have one authoritative primary region; cross-region replicas are not promoted automatically.
- Failover requires Incident Commander and Risk Owner approval, a recorded incident, confirmation of database consistency, and a documented venue-state reconciliation plan.
- After recovery, the system starts in `RECOVERY_HOLD`: read-only observation and reconciliation are allowed; new risk-increasing commands, strategy deployment, credential rotation, and live activation remain blocked.
- Re-enable requires: database/ledger invariants pass; order/fill/balance/position reconciliation completed or explicitly dispositioned; market-data freshness verified; risk configuration and signing keys verified; smoke/fault checks pass; and two distinct authorized approvers sign the scope-specific release.
- If venue state cannot be established, orders with uncertain outcome remain `UNKNOWN`; the system must not retry an exposure-increasing command.
- Recovery evidence includes recovery point, elapsed time, data-loss assessment, reconciliation result, approvals, artifact/config digests, and follow-up actions.
