# NFR, Observability and Capacity Standard

## Availability targets and measurement

Production control plane: 99.95% monthly availability; critical internal risk/OMS service health: 99.99% monthly. A 30-day month therefore permits at most 21.6 minutes and 4.32 minutes of unavailability respectively. Venue downtime, client-side failures, and approved maintenance announced at least 72 hours in advance are excluded; provider failures inside the platform boundary are not excluded. Availability is measured from externally probed synthetic transactions and server-side successful eligible requests. A request is successful only when the service returns the documented successful response within its latency objective. Client 4xx responses are excluded; server 5xx, timeout, and readiness failure count as unavailable.

## Measurement boundaries and maintenance

Availability denominator is the count of eligible synthetic and production requests during the calendar month; each request is weighted equally within its endpoint class. A request succeeds only if it returns the documented successful result within that class's latency objective. The external synthetic probe runs at least once per minute from two independent locations; if either probe fails, the interval is unavailable unless the probe itself is independently proven faulty. The monthly SLO is reported separately for public control-plane APIs and the internal risk/OMS critical path; internal service health is measured using dependency-aware synthetic command flows, not process uptime alone.

Planned maintenance is excluded only when announced at least 72 hours ahead, approved, and limited to a maximum of two hours per calendar month. Excess maintenance time counts as unavailable. Unplanned maintenance and cloud/provider failures inside the platform boundary count against the SLO. No availability exclusion may be retroactively applied without an incident record and independent owner review.

Safety invariants are not traded against availability: unauthorized financial command acceptance target is zero; every accepted risk-increasing command must have a durable risk decision and audit event; a missing/stale authoritative input causes a fail-closed decision. These are invariant checks, not statistical SLOs.

## Latency targets

API read p95 <= 200 ms; API mutation acknowledgement p95 <= 300 ms; internal risk evaluation p95 <= 20 ms; OMS state transition p95 <= 50 ms. These targets apply at the stated baseline load and are not guarantees about external venues. Percentiles are calculated over rolling five-minute windows and reported by endpoint class, environment, and region. Mutation acknowledgement means durable command acceptance, not venue acknowledgement or fill.

## Error budgets and alert thresholds

Monthly error budget is 0.05% for the control plane and 0.01% for internal risk/OMS. At 50% budget consumption, freeze nonessential releases and require service-owner review; at 75%, require an incident review and approved remediation before promotion; at 100%, stop routine production releases until the SLO is restored or the owner records a time-bounded exception. Page immediately for any unauthorized order, risk/OMS authority loss, live credential exposure, system halt failure, or material ledger/reconciliation integrity break. Page the on-call operator when any of these conditions persists for two minutes: control-plane error rate >2% over five minutes; risk/OMS error rate >0.5% over five minutes; p95 latency >2x target for five minutes; event dispatch oldest-record age >30 seconds; required market-data freshness limit breached; database primary unavailable; or a material reconciliation break remains unresolved. Queue lag and data-freshness thresholds may be tightened by venue policy, never relaxed by an adapter.

## Capacity baseline

Reference production cluster must sustain 2,000 market events/sec, 100 order commands/sec, 500 concurrent strategies, 50 concurrent operators, and 20 venues without breaching the stated latency targets. Capacity tests must include 60 minutes at the stated baseline, 5x burst for 15 minutes, and 20x burst for 60 seconds. The test dataset uses market events up to 1 KiB serialized each, command envelopes up to 64 KiB, and API bodies up to the 1 MiB request limit. After each burst, queued work must return to the pre-burst lag within 15 minutes without dropping authoritative events or violating venue rate limits. Tests report throughput, p50/p95/p99 latency, error rate, CPU, memory, database IOPS/connections/WAL, queue age, and recovery time. Payloads above the defined limits are rejected before domain processing.

## Recovery objectives

Critical control plane RTO <= 30 minutes; RPO <= 5 minutes. Research services RTO <= 4 hours; RPO <= 24 hours. Recovery objectives are measured from declared incident start to restored service and validated by drills.

## Observability

Every request and event carries correlation and causation identifiers. Metrics include request rate, error rate, latency, risk rejects, order states, venue acknowledgements, reconciliation breaks, data freshness, queue depth and oldest-message age, database health, resource saturation, and model health. Dashboards show SLO burn rate, error budget remaining, market/venue/environment segmentation, and deployment annotations. Logs are structured, redact secrets and sensitive account data, and preserve command/event identifiers. Alerts must have an owner, severity, runbook, and actionable threshold; alerts without a response action are not production alerts.

Alerts are severity-based. SEV-1 indicates active risk of unauthorized or uncontrolled financial action or loss of authoritative state. SEV-2 indicates major degradation requiring immediate operator intervention. SEV-3 is contained service degradation. SEV-4 is non-urgent defect or maintenance issue.

## Capacity scaling

Horizontal scaling is permitted for stateless API and worker components. PostgreSQL scales vertically first, then read replicas for non-authoritative analytics. Financial writes remain on the primary authority database.
