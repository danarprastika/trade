# Runbook: SEV-1 Incident Response

**Owner:** Platform Operations (primary on-call), Risk Operations (Risk Owner)
**Alert routes:** `https://runbooks.trade.internal/incident-sev1-response`
**Authority:** 08_NFR_OBSERVABILITY_AND_CAPACITY.md §Observability — "SEV-1 indicates active risk of unauthorized or uncontrolled financial action or loss of authoritative state."
**10_OPERATIONS_AND_DISASTER_RECOVERY.md §Incident command** — "SEV-1 incidents require an Incident Commander, Technical Lead, Risk Owner, and Communications Owner."

> This is an executable procedure, not a summary. Every step is numbered and
> ordered. Do not skip steps. If a step cannot be completed, record why and
> escalate — an incomplete step is a finding, not a detail.
>
> 24_ENTERPRISE_RELEASE_STANDARD.md §2 Operational readiness: "Every production
> alert has an owner and executable response procedure."

---

## 0. Preconditions — before you begin

- [ ] You have authority to act. Infrastructure privilege is JIT and expires automatically (06_SECURITY_AND_ACCESS_CONTROL.md §5).
- [ ] If the identity broker or authorization service is down, you may use break-glass: two-person release, incident ID, reason, maximum 30-minute session. Break-glass "cannot bypass Risk Engine, OMS, ledger, or live activation gates" (06 §5).

---

## 1. Declare the incident

1. Open an incident record. Record: incident ID, declaring identity, UTC timestamp, the triggering alert name, and the affected environment.
2. Assign the four required roles (10 §Incident command):
   - **Incident Commander** — controls coordination.
   - **Technical Lead** — controls technical investigation.
   - **Risk Owner** — controls trading halt/re-enable decisions.
   - **Communications Owner** — controls external communication.
3. The same identity MUST NOT hold both Incident Commander and Risk Owner. 06_SECURITY_AND_ACCESS_CONTROL.md §3: "The initiating actor and approver must be distinct identities."
4. Announce the incident channel and the bridge.

## 2. Activate the system halt

Follow the numbered procedure in 10_OPERATIONS_AND_DISASTER_RECOVERY.md §Immediate response to uncontrolled behavior:

1. **Activate system halt.**
2. **Block new risk-increasing commands.**

Notes:
- Halt activation is immediately effective and monotonic in severity (17_CONFIGURATION_AND_RISK_POLICY.md §5). A halt cannot be cleared by a lower-authority command, a stale configuration, a feature flag, a model output, or recovery automation (25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_PROFILE.md §3 invariant 9).
- If the halt activation call fails, the `SystemHaltFailure` alert fires as SEV-1. **Treat trading as halted regardless of the control-plane response.** Use break-glass per 06 §5 if the normal path is unavailable.
- Emergency halt is always available to authorized halt operators and does not require a second approver; **re-enable does** (06 §3).

## 3. Preserve logs, audit records, and deployment state

3. Preserve all logs, audit records, and deployment state before any remediation changes them.
4. Snapshot the current deployment: artifact digests, configuration fingerprint, feature-flag state, and Terraform plan output.
5. Verify the audit chain is intact at this moment: `verifyAuditChain()` must succeed. A chain break is itself a SEV-1 security event (22_AUDIT_INTEGRITY_AND_EVIDENCE.md §3).
6. Do NOT delete, truncate, or repair audit evidence. 24_ENTERPRISE_RELEASE_STANDARD.md §16 prohibits "silent audit deletion or mutation".

## 4. Establish whether venue state is known

7. For every in-flight order, determine whether the venue outcome is known.
8. Any order whose submission outcome cannot be determined MUST be recorded as `UNKNOWN` in the OMS (04_TRADING_DOMAIN_AND_RISK.md §Order lifecycle).
9. **Do not retry an exposure-increasing order whose outcome is unknown** (10 §Recovery topology: "the system must not retry an exposure-increasing command").

## 5. Reconcile orders, fills, balances, and positions

10. Run reconciliation across orders, fills, balances, positions, and fees (05_PERSISTENCE_EVENTING_RECONCILIATION.md §Reconciliation).
11. Classify every difference with explicit severity, assign an owner, attach evidence, and start the aging SLA.
12. A material unresolved break BLOCKS the affected risk-increasing scope. It does not merely warn.

## 6. Restore authoritative state

13. Restore the authoritative state. If the PostgreSQL primary is unavailable: 25 §6 — "Stop authoritative mutations; do not substitute Redis, event bus, or local memory as source of truth."
14. Promote the in-region HA standby if required. Do **not** promote across regions automatically (10 §Recovery topology: "cross-region replicas are not promoted automatically").
15. If cross-region recovery is required, switch to the `regional-failover-recovery-hold` runbook. Recovery enters `RECOVERY_HOLD`.

## 7. Validate risk and data health

16. Verify the Risk Engine is authoritative and evaluating within its latency objective (p95 ≤ 20 ms per 08 §Latency targets).
17. Verify the OMS state machine is consistent and there are no `UNKNOWN` orders.
18. Verify market-data freshness within the configured per-instrument limits.
19. Verify the ledger is balanced and append-only (05 §Ledger: "Ledger entries are append-only and balanced. Corrections are represented by compensating entries, never destructive updates.").
20. Verify the audit chain is continuous from the last signed checkpoint (22 §7).

## 8. Re-enable in the narrowest affected scope

21. Re-enable ONLY the narrowest affected scope. Never the whole environment by default.
22. The Risk Owner controls the re-enable decision (10 §Incident command).
23. Re-enable requires (10 §Recovery topology):
    - database/ledger invariants pass;
    - order/fill/balance/position reconciliation completed **or explicitly dispositioned**;
    - market-data freshness verified;
    - risk configuration and signing keys verified;
    - smoke/fault checks pass;
    - **two distinct authorized approvers sign the scope-specific release**.
24. Clear any halts in descending severity order only, each with the owning authority and a recorded reason (17 §5).

## 9. Produce incident evidence and corrective actions

25. Produce the incident evidence package: full timeline, root-cause analysis, evidence digests, and verified remediation (24_ENTERPRISE_RELEASE_STANDARD.md §2 Incident management: "Every SEV-1/2 has complete timeline, owner, root-cause analysis, and verified remediation").
26. Record corrective actions with owners and expiry dates. 25 §7: "All exceptions must have a scope, approver, expiry, compensating control, and audit record."
27. If the incident used break-glass, complete the post-incident review **within one business day** (06 §5).
28. File the audit checkpoint if a key rotation or high-impact security event occurred (23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §6).

---

## Escalation

| Condition | Escalate to |
|---|---|
| Halt cannot be activated | Security on-call; break-glass (06 §5) |
| Audit chain break | Security on-call; 22_AUDIT_INTEGRITY_AND_EVIDENCE.md §3 |
| Live credential exposure | Security on-call; `key-rotation` runbook |
| Cross-region recovery needed | Incident Commander + Risk Owner; `regional-failover-recovery-hold` runbook |
| Re-enable disagreement | Owner; 18_GOVERNANCE_DATA_AND_COMPLIANCE.md §1 |

## Prohibited during a SEV-1

- Do not bypass the Risk Engine, OMS, reconciliation, ledger, or live activation gates (06 §5, 24 §16).
- Do not use AI/model output to authorize any action (18 §4; 25 §3 invariant 1).
- Do not apply unexplained Terraform drift (24 §5).
- Do not delete or mutate audit evidence.
- Do not clear a halt with a single identity.
