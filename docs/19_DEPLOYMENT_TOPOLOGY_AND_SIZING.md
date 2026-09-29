# Deployment Topology and Capacity Sizing

## 1. Reference deployment

The production reference deployment uses a private network with separate edge, application, data, and management zones. Public ingress terminates at a managed WAF/load balancer. Go API instances and workers run as non-root OCI containers on a managed container platform. PostgreSQL is a managed highly available primary with synchronous protection within the primary region and encrypted WAL/backups replicated to a recovery region. Secrets are held in a managed secret store. Telemetry is exported through OpenTelemetry collectors to metrics, logs, and traces backends.

The initial platform is a modular monolith for authoritative Go domains, with independently scalable workers for research, backtesting, event dispatch, and venue adapters. Separate deployments do not imply separate policy authority. Only the Go domain boundary can authorize financial state transitions.

## 2. Environment boundaries

Dev/test/staging/paper/shadow/live have separate accounts/projects, network policies, databases, keys, service identities, and venue credentials. Live secrets are not readable by lower environments. Production administration uses a dedicated identity and audited just-in-time access. Live deployment requires protected approval and signed immutable artifacts.

## 3. Initial sizing envelope

The reference capacity envelope is 2,000 normalized market events/sec, 100 order commands/sec, 500 concurrent strategies, 50 concurrent operators, and 20 connected venues. Load tests include sustained baseline, 5x burst for 15 minutes, and 20x burst for 60 seconds. The envelope is a measurable acceptance target, not a claim that a particular cloud SKU guarantees it.

Initial production sizing starts with a three-zone primary region and a separately secured recovery region. The minimum application footprint is three Go API replicas (2 vCPU / 4 GiB each), two event-dispatcher replicas (2 vCPU / 4 GiB each), and at least one isolated adapter worker per enabled venue (2 vCPU / 4 GiB each, concurrency capped by venue rules). Python research/backtest workers use a separate quota-controlled pool and cannot consume reserved API, risk, OMS, or adapter capacity. Begin PostgreSQL with a managed HA primary/standby pair, each at least 8 vCPU / 32 GiB RAM and 1 TiB encrypted SSD storage, with automated WAL archiving and connection pooling; benchmark-derived IOPS and storage growth determine final provisioned capacity. If NATS JetStream is enabled, deploy three nodes across three zones with replicated durable streams; it is not required for the initial transactional-outbox implementation. Redis is optional cache-only infrastructure and must not be a dependency for risk authorization or order correctness.

Autoscale stateless API/worker pools on sustained CPU >65% or queue lag above the documented threshold for five minutes, with scale-in only after ten minutes below 35% and with in-flight work drained. API replicas have a minimum of three in production. Adapter concurrency and request rates are governed by per-venue limits, not generic autoscaling. PostgreSQL does not autoscale blindly: storage, CPU, memory, connections, IOPS, WAL rate, and replica lag are reviewed against benchmark and growth forecasts. These are initial minimums, not a claim that a particular cloud SKU guarantees the capacity envelope. PostgreSQL CPU, memory, IOPS, connection count, WAL throughput, queue lag, and storage growth must be measured under the stated envelope before live activation.

## 4. Scaling and isolation

Scale stateless APIs horizontally. Scale research workers independently with quotas so experiments cannot starve risk/OMS workloads. Apply per-venue concurrency and rate limits. Use bounded queues and backpressure; reject or defer noncritical research work before allowing it to affect authoritative paths. Financial writes remain on the authoritative database primary. Read replicas are for explicitly stale-tolerant analytics only.

## 5. Availability and failure behavior

The internal critical path targets remain defined in the NFR document. Venue availability is external and excluded from platform availability calculations. On loss of database authority, risk state, or required market data, risk-increasing commands fail closed. On broker/event-bus failure, committed outbox records remain durable and dispatch resumes idempotently. On a venue timeout with uncertain submission, OMS records `UNKNOWN` and reconciliation resolves the state before retry.

## 6. Capacity acceptance

Before G10, publish benchmark environment, dataset, workload model, software/configuration digest, latency percentiles, saturation points, error rates, queue lag, database metrics, and recovery behavior. All stated latency and capacity targets must pass at baseline and burst loads. Capacity shortfall requires a documented remediation and retest; it cannot be waived by documentation.
