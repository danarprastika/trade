# Runbook: SEV-2 Incident Response

**Owner:** Platform Operations (primary on-call)
**Alert routes:** `https://runbooks.trade.internal/incident-sev2-response`
**Authority:** 08_NFR_OBSERVABILITY_AND_CAPACITY.md §Observability — "SEV-2 indicates major degradation requiring immediate operator intervention."
**Trigger:** The 08 §4 conditions "persists for two minutes": control-plane error rate >2%/5m; risk/OMS error rate >0.5%/5m; p95 latency >2x target for 5m; event dispatch oldest-record age >30s; market-data freshness breach; database primary unavailable; material reconciliation break unresolved.

> 24_ENTERPRISE_RELEASE_STANDARD.md §2: "Every production alert has an owner and
> executable response procedure." 08 §Observability: "alerts without a response
> action are not production alerts."

---

## 1. Acknowledge and classify

1. Acknowledge the page within 5 minutes. Record: incident ID, alert name, environment, and UTC timestamp.
2. Confirm the severity classification. **Escalate to the `incident-sev1-response` runbook immediately if any of these is true:**
   - unauthorized order accepted;
   - Risk Engine or OMS authority lost;
   - live credential exposure;
   - system halt failure;
   - material ledger/reconciliation integrity break;
   - any condition that could produce an uncontrolled financial action.
3. Record the affected environment, service, market scope, and venue scope.

## 2. Establish the blast radius

4. Determine whether authoritative state is affected. It is not safe to assume degradation is non-authoritative.
5. Check the Risk Engine and OMS are still authoritative (25 §6). If either is not, this is a SEV-1.
6. Check whether risk-increasing commands are being accepted. If the platform cannot establish authoritative state, it must deny new risk-increasing activity (01_SYSTEM_ARCHITECTURE.md §6).

## 3. Follow the immediate-response procedure

7. If behavior is uncontrolled, execute 10_OPERATIONS_AND_DISASTER_RECOVERY.md §Immediate response to uncontrolled behavior in order:
   1. Activate system halt.
   2. Block new risk-increasing commands.
   3. Preserve logs, audit records, and deployment state.
   4. Establish whether venue state is known.
   5. Reconcile orders, fills, balances, and positions.
   6. Restore authoritative state.
   7. Validate risk and data health.
   8. Re-enable in the narrowest affected scope.
   9. Produce incident evidence and corrective actions.
8. If behavior is contained, continue to step 4 of this runbook without halting. Do not halt reflexively — a halt is a real operational state with its own re-enable cost.

## 4. Diagnose by symptom

### 4a. Control-plane error rate >2% over five minutes
9. Check database health first: primary availability, connection saturation, lock waits, and statement timeouts.
10. Check dependency failures: identity provider, authorization policy service, secret store.
11. 25 §6: "Identity provider unavailable | Deny new privileged sessions and mutations". "Authorization policy stale or unverifiable | Fail closed for privileged and risk-increasing commands; alert policy owner."
12. If the fault is an external venue, note that venue downtime is excluded from platform availability (08 §1) and does not consume the error budget.

### 4b. p95 latency above 2x target
13. Identify the endpoint class: read (200 ms target), mutation acknowledgement (300 ms), risk evaluation (20 ms), OMS transition (50 ms) — 08 §Latency targets.
14. Determine whether the latency is in the application tier or the database tier.
15. Do NOT clear latency pressure by relaxing risk checks. 17_CONFIGURATION_AND_RISK_POLICY.md §3: limits are owner-configured and reviewed; "No model or strategy may modify its own risk policy".

### 4c. Event dispatch oldest-record age >30 seconds
16. Confirm committed outbox records are durable (05 §Outbox: the dispatcher reads with `FOR UPDATE SKIP LOCKED` and marks delivery state).
17. Check dispatcher health, database lock contention, and consumer saturation.
18. Apply bounded backpressure. Do not drop events. 25 §6: "no event loss or duplicate domain transition."
19. Sustained lag above 5 s for 10 minutes is the documented NATS JetStream introduction threshold (23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §6). **Evaluate and record the gate; do not enable NATS without the recorded evidence.**

### 4d. Market-data freshness breach
20. Mark the affected feed unhealthy.
21. Deny commands requiring the affected data (16_MARKET_DATA_AND_VENUE_ADAPTERS.md §2).
22. **Do not substitute a cached value.** If a cached value is displayed, label its age and source.
23. Retain provenance for the affected observations.

### 4e. Database primary unavailable
24. Escalate to SEV-1 handling. 25 §6: "PostgreSQL primary unavailable | Stop authoritative mutations; do not substitute Redis, event bus, or local memory as source of truth."
25. Promote the in-region standby. Cross-region promotion requires Incident Commander AND Risk Owner approval and a recorded incident (10 §Recovery topology).

### 4f. Material reconciliation break unresolved
26. Open a reconciliation case with classification, owner, severity, evidence, and aging SLA.
27. Keep the affected risk-increasing scope blocked until dispositioned.

## 5. Mitigate and verify

28. Apply the narrowest mitigation that restores the service. Prefer reverting to a previously verified immutable artifact over forward-fixing in place during an incident (24_ENTERPRISE_RELEASE_STANDARD.md §15).
29. Verify recovery against the alert's own threshold, not against "it looks better".
30. Confirm no authoritative events were dropped and no order was duplicated.

## 6. Restore normal operation

31. Re-enable only after: risk and data health validated, reconciliation dispositioned, and smoke checks pass.
32. If the environment is in `RECOVERY_HOLD`, follow `regional-failover-recovery-hold` step 8 — re-enable requires two distinct authorized approvers.

## 7. Record evidence

33. Record: timeline, root cause, evidence digests, corrective actions with owners and expiry.
34. Check the error budget (08 §4: 0.05% control plane, 0.01% risk/OMS). At 50% consumption freeze nonessential releases; at 75% require incident review and approved remediation; at 100% stop routine production releases.
35. 25 §7: "SLO exclusions cannot be used to hide failed releases, incidents, or recovery exercises."

## 8. Escalate to SEV-1 if

- authoritative state cannot be established;
- an unauthorized order was accepted;
- a halt cannot be activated;
- credential or signing-key compromise is suspected;
- a cross-region failover is required.
