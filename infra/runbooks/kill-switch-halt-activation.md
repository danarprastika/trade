# Runbook: Kill-Switch and Halt Activation

**Owner:** Risk Operations (Risk Owner) — Platform Operations supports
**Alert routes:** `https://runbooks.trade.internal/kill-switch-halt-activation`
**Authority:** 10_OPERATIONS_AND_DISASTER_RECOVERY.md §Immediate response to uncontrolled behavior, steps 1-2; 04_TRADING_DOMAIN_AND_RISK.md §Halt hierarchy; 17_CONFIGURATION_AND_RISK_POLICY.md §5.

> **An emergency halt is always available and does NOT require a second approver.**
> **A re-enable DOES require two-person approval** (06_SECURITY_AND_ACCESS_CONTROL.md §3).
> This asymmetry is deliberate: halting is always the safer state, so it must never
> be blocked by a process requirement.

---

## 0. Halt authority model

Halt precedence (04 §Halt hierarchy), strongest first:

```
SYSTEM_HALT > VENUE_HALT > MARKET_HALT > STRATEGY_HALT > ACCOUNT_HALT
```

- "A higher-level halt cannot be bypassed by a lower-level enable command."
- "Halt activation is immediately effective and monotonic in severity" (17 §5).
- 25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_PROFILE.md §3 invariant 9: "A halt is monotonic: a lower-privilege action, stale configuration, feature flag, model output, or recovery automation cannot silently clear it."
- 25 §6: "Emergency halt remains available independently of AI services and noncritical UI components."

---

## 1. Decide to halt

1. Halt when any of the following is true. Do not wait for certainty:
   - the platform cannot establish authoritative state (01_SYSTEM_ARCHITECTURE.md §6);
   - the Risk Engine is not authoritative;
   - the OMS is not authoritative;
   - any order is in `UNKNOWN` state and venue state cannot be established;
   - a material reconciliation break is unresolved in the affected scope;
   - required market data is stale or degraded for the affected scope;
   - an unauthorized order was accepted;
   - market data is contradictory (25 §6: "Market data stale or contradictory");
   - a configuration signature is invalid and freshness cannot be proven;
   - credential or signing-key compromise is suspected;
   - an operator observes behaviour they cannot explain. **An unexplained anomaly is a halt condition, not a judgement call.**

2. Halt at the **narrowest scope that addresses the risk**, then widen if needed. A `MARKET_HALT` on one market is preferable to a `SYSTEM_HALT`.

## 2. Activate the halt

3. **Prefer the narrowest applicable halt level**, in this order: ACCOUNT → STRATEGY → MARKET → VENUE → SYSTEM.
4. Record with the halt:
   - the halt level and scope;
   - the activating identity and role;
   - UTC timestamp;
   - the reason;
   - the triggering alert or observation;
   - the evidence reference.
5. Verify the halt is effective: submit a test risk-increasing command in the halted scope and confirm it is **rejected**.
6. If the halt activation call fails or the verification command is accepted, treat trading as halted regardless. The `SystemHaltFailure` alert fires as SEV-1. Use break-glass per 06_SECURITY_AND_ACCESS_CONTROL.md §5:
   - two-person release;
   - incident ID and reason recorded;
   - maximum 30-minute session;
   - immediate security alerting.
   - Break-glass **cannot** bypass the Risk Engine, OMS, ledger, or live activation gates.
7. Emergency halt is available independently of AI services, the operator UI, and notification paths. If the UI is down, the halt path still works.

## 3. Block new risk-increasing commands

8. Confirm that new risk-increasing commands are rejected in the halted scope.
9. Confirm that **read-only** observation continues. A halt is not a shutdown: operators must still be able to observe, export evidence, and reconcile.
10. Confirm that risk-**reducing** actions follow the separately defined, audited safe-reduction policy and cannot increase net exposure (17 §4: "Risk-reducing actions may proceed only through a separately defined, audited safe-reduction policy and must not increase net exposure").

## 4. Preserve evidence

11. Preserve logs, audit records, and deployment state (10 §Immediate response step 3).
12. Verify the audit chain. A chain break is a SEV-1 security event in its own right (22_AUDIT_INTEGRITY_AND_EVIDENCE.md §3).
13. Do not delete, truncate, or repair audit evidence.

## 5. Resolve UNKNOWN orders

14. Establish whether venue state is known for every in-flight order (10 §Immediate response step 4).
15. Any order whose outcome cannot be determined is `UNKNOWN` and stays `UNKNOWN` until evidence resolves it (04 §Order lifecycle).
16. **Never retry an exposure-increasing command while an order is `UNKNOWN`** (10 §Recovery topology).

## 6. Reconcile

17. Reconcile orders, fills, balances, positions, and fees (10 §Immediate response step 5).
18. Open reconciliation cases with classification, owner, severity, evidence, and aging SLA.

## 7. Restore authoritative state and validate

19. Restore authoritative state (10 §Immediate response step 6). Never substitute Redis, the event bus, or local memory for the authoritative database (25 §6).
20. Validate risk and data health (step 7):
    - Risk Engine authoritative and within p95 20 ms;
    - OMS consistent, no `UNKNOWN` orders;
    - market-data freshness within limits;
    - ledger balanced and append-only;
    - audit chain continuous from the last signed checkpoint.

## 8. Clear the halt

21. Clearing a halt requires (17 §5):
    - the **owning authority** for that halt level;
    - a **recorded reason**;
    - **health checks** passed;
    - **reconciliation status** clean or explicitly dispositioned;
    - a **two-person approval for system-wide live re-enable**.
22. Clear halts **only from the lowest applicable level upward**, and only after the level above it has been cleared by its own owning authority. Never clear a higher level first.
23. Do not clear a halt to resolve an alert. Clear it because the underlying condition is resolved.
24. Emergency halt remains available at all times, including during re-enable.

## 9. Re-enable narrowly

25. Re-enable in the **narrowest affected scope** (10 §Immediate response step 8).
26. Record the re-enable as an auditable event with: scope, authorizing identities, reason, health evidence, reconciliation status, and UTC timestamp.
27. System-wide live re-enable additionally requires a successful reconciliation snapshot, current eligibility, and gates G0-G10 PASS (21_ZERO_TRUST_SSO_AND_AUTHORIZATION.md §6).

## 10. Produce evidence

28. Produce the incident evidence and corrective actions (10 §Immediate response step 9).
29. Exercise schedule: **quarterly kill-switch exercise** (10 §Drill schedule). The exercise records expected safety state, observed state, evidence digest, duration, operator, and corrective action (24 §13).

## Prohibited

- Clearing a higher-level halt with a lower-level enable command.
- Clearing a halt with a single identity.
- Clearing a halt because a model, feature flag, or AI output suggests it (25 §3 invariant 9).
- Clearing a halt while an `UNKNOWN` order exists.
- Clearing a halt while a material reconciliation break is unresolved in the affected scope.
- Using break-glass to bypass financial authorization.
