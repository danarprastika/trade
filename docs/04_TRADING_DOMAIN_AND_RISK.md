# Trading Domain and Risk

## Strategy lifecycle

`DRAFT -> REVIEW -> BACKTESTED -> SIMULATION -> PAPER -> SHADOW -> APPROVED -> DEPLOYED -> PAUSED -> RETIRED`.

Transitions are explicit commands and produce immutable audit events.

## Order lifecycle

`CREATED -> RISK_PENDING -> RISK_APPROVED -> SUBMITTING -> ACKNOWLEDGED -> PARTIALLY_FILLED -> FILLED` with terminal alternatives `CANCELLED`, `REJECTED`, `EXPIRED`, and `UNKNOWN`.

`UNKNOWN` is mandatory when a submission timeout prevents determination of venue outcome. The system reconciles before any retry that could duplicate exposure.

## Risk gate

Every risk-increasing order passes deterministic checks for:

- account and environment authorization
- strategy deployment status
- instrument eligibility
- venue availability
- price and quantity precision
- notional limits
- position limits
- exposure limits
- concentration limits
- leverage limits
- loss and drawdown controls
- stale market-data controls
- duplicate-order detection
- rate and throttle limits
- kill/halt state

A single failed mandatory control rejects the order.

## Halt hierarchy

`SYSTEM_HALT > VENUE_HALT > MARKET_HALT > STRATEGY_HALT > ACCOUNT_HALT`.

A higher-level halt cannot be bypassed by a lower-level enable command. Recovery requires the owning authority, validation of health conditions, and an auditable re-enable event.

## Portfolio integrity

Positions are derived from validated fills. Cash and financial balances derive from ledger facts and reconciled venue records. P&L is a projection that can be rebuilt from ledger and market data.

## Live safety defaults

Live is disabled by default. Credentials are unavailable until activation. A release must pass all technical gates and receive explicit owner authorization before live submission is enabled.
