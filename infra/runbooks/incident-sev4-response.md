# Runbook: SEV-4 Non-Urgent Defect / Maintenance

**Owner:** Platform Operations
**Alert routes:** `https://runbooks.trade.internal/incident-sev4-response`
**Authority:** 08_NFR_OBSERVABILITY_AND_CAPACITY.md §Observability — "SEV-4 is non-urgent defect or maintenance issue."

> SEV-4 is tracked to the next scheduled maintenance window. It must never be used
> to defer a condition that is actually SEV-1/2/3 — reclassify before proceeding.

---

## 1. Classify and queue

1. Acknowledge within the next scheduled maintenance window.
2. Open a ticket. Record: alert name, environment, service, first-seen UTC, owner, and the maintenance window it is assigned to.
3. Confirm the condition is genuinely non-urgent:
   - authoritative state, risk authorization, order correctness, and halt capability are all intact;
   - no error-budget consumption trend;
   - no compliance or eligibility impact.
4. If any of these fail, reclassify to SEV-3, SEV-2, or SEV-1 and use that runbook.

## 2. Schedule

5. Assign the defect to a maintenance window that satisfies the notice and duration limits in 08_NFR_OBSERVABILITY_AND_CAPACITY.md §Measurement boundaries:
   - announced at least **72 hours** ahead;
   - approved;
   - limited to a **maximum of two hours per calendar month**.
6. Excess maintenance time counts as **unavailable**. Do not plan work that exceeds the two-hour monthly allowance.
7. Unplanned maintenance and cloud/provider failures inside the platform boundary count against the SLO.

## 3. Plan the change

8. Prepare: ticket, code review, automated evidence, deployment plan, rollback plan, authorized release (10 §Change management).
9. Use immutable artifacts. Rollback restores the previous artifact AND configuration snapshot (09_TESTING_AND_RELEASE_EVIDENCE.md §Release strategy).
10. Database migrations must be backward-compatible with the immediately previous release. Use expand/contract discipline (24_ENTERPRISE_RELEASE_STANDARD.md §5).
11. Record the configuration fingerprint before deployment and after deployment (24 §5).

## 4. Execute

12. Announce the window. Open the change record.
13. Execute the approved plan only. Any deviation is a new change requiring approval.
14. If the window will exceed two hours or the plan requires halting trading, stop and re-plan — a SEV-4 change may not become an unannounced outage.
15. Emergency changes are permitted but require retrospective review **within one business day** (10 §Change management).

## 5. Verify and close

16. Verify against the alert's own threshold.
17. Verify audit-chain continuity after the deployment (22_AUDIT_INTEGRITY_AND_EVIDENCE.md §3: verification runs "hourly and after restore/deployment").
18. Record the maintenance minutes consumed this calendar month against the 120-minute allowance.
19. Close the ticket with root cause, fix, evidence digests, and the maintenance record.
