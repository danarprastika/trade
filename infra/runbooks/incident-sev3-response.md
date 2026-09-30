# Runbook: SEV-3 Incident Response (contained degradation)

**Owner:** Platform Operations
**Alert routes:** `https://runbooks.trade.internal/incident-sev3-response`
**Authority:** 08_NFR_OBSERVABILITY_AND_CAPACITY.md §Observability — "SEV-3 is contained service degradation."

> SEV-3 means the degradation is contained: authoritative state, risk authorization,
> order correctness, and halt capability are all intact. If any of those four is in
> question, this is a SEV-1 — use `incident-sev1-response`.

---

## 1. Triage

1. Acknowledge within 4 hours (08 §Observability routing: SEV-3 response is next-business-day contained, tracked to closure).
2. Open a ticket. Record: alert name, environment, service, first-seen UTC, owner.
3. Confirm containment. Verify explicitly:
   - the Risk Engine is authoritative;
   - the OMS is authoritative;
   - no order is in `UNKNOWN` state;
   - no halt is active;
   - the ledger is balanced.
4. If any containment check fails, stop and escalate to SEV-1.

## 2. Diagnose

5. Identify the affected component and the blast radius.
6. Determine whether the degradation is capacity-related, configuration-related, or dependency-related.
7. Capacity-related findings go to the capacity evidence record (19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §6): "Capacity shortfall requires a documented remediation and retest; it cannot be waived by documentation."
8. Configuration-related findings: check drift. 24_ENTERPRISE_RELEASE_STANDARD.md §5: unexplained drift is an operational incident. Compare the running configuration fingerprint against the recorded release snapshot.

## 3. Common SEV-3 conditions

### 3a. Error budget consumption (50% / 75% / 100%)
9. 08 §4: at **50%** consumption — freeze nonessential releases and require service-owner review.
10. At **75%** — require an incident review and approved remediation before promotion.
11. At **100%** — stop routine production releases until the SLO is restored or the owner records a time-bounded exception.
12. 25 §7: every exception must have a scope, approver, expiry, compensating control, and audit record. Exceptions may not be used to hide a failed release or incident.

### 3b. Storage headroom
13. Review storage growth against benchmark and growth forecasts (19 §3).
14. Plan a reviewed increase or a data lifecycle action.
15. **Do not delete retained data to make room.** 05_PERSISTENCE_EVENTING_RECONCILIATION.md §Data retention: "Deletion never removes records under legal hold or financial audit retention." Audit and financial records are retained 7 years.

### 3c. Infrastructure drift
16. Obtain the drift plan. Identify the change source: manual console change, an unreviewed apply, or an external actor.
17. If the change is explained and has valid evidence, reconcile with a reviewed apply.
18. If unexplained, treat as an incident, preserve evidence, and audit access. 06_SECURITY_AND_ACCESS_CONTROL.md §1: "ordinary application nodes cannot administer infrastructure."
19. Block further privileged changes until resolved (24 §5).

### 3d. Throughput below the capacity envelope
20. 08 §Capacity baseline reference envelope: 2,000 market events/sec, 100 order commands/sec, 500 concurrent strategies, 50 concurrent operators, 20 venues.
21. **Confirm there is actually load before treating this as a fault.** The envelope is a capacity acceptance target, not a traffic guarantee (19 §3).
22. If load is present and throughput is short, record it as a capacity finding and schedule remediation plus retest.

### 3e. Post-burst queue recovery exceeded 15 minutes
23. 08 §Capacity baseline: "After each burst, queued work must return to the pre-burst lag within 15 minutes without dropping authoritative events or violating venue rate limits."
24. Verify no authoritative events were dropped. If any were, escalate.

## 4. Remediate

25. Prefer a reviewed change through the normal release path. Emergency changes require retrospective review within one business day (10_OPERATIONS_AND_DISASTER_RECOVERY.md §Change management).
26. Any production change requires: a ticket, code review, automated evidence, a deployment plan, a rollback plan, and an authorized release.

## 5. Verify and close

27. Verify the alert threshold is genuinely cleared and has not simply moved to a different signal.
28. Record: root cause, fix, evidence digests, and whether the error budget was consumed.
29. Close the ticket only when the containment checks in step 3 all pass again.
