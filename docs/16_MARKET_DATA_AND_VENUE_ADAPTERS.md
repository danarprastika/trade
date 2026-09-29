# Market Data and Venue Adapter Standard

## 1. Instrument identity

Every tradable instrument has a platform `instrument_id`, market class, canonical base/quote or contract definition, venue symbol mapping, price/quantity increments, minimum order constraints, trading status, and effective timestamps. Venue symbols are never treated as globally unique identifiers. Instrument mapping changes are versioned and audited.

## 2. Market data normalization

Adapters normalize trades, quotes, candles, order-book updates, funding/fee metadata, trading status, and venue timestamps into versioned contracts. Every observation carries source venue, instrument ID, source timestamp, receive timestamp, sequence where supplied, and quality flags. Raw vendor payloads may be retained for diagnostics under retention controls; normalized records are the internal consumption contract.

Market data is considered stale when its age exceeds the instrument/market-specific freshness threshold configured in the environment policy. Missing sequence continuity, invalid timestamps, crossed/invalid books, unsupported precision, or a feed-health breach marks the stream degraded. Risk-increasing commands are rejected when required inputs are stale or degraded. The platform never substitutes a cached value without labeling its age and source.

## 3. Market sessions and calendars

Each instrument references a market calendar and venue status source. Session open/close, holidays, auctions, halts, and contract roll rules are represented explicitly. The adapter must not infer tradability from a wall-clock schedule alone when venue status is available. Calendar versions and exceptional closures are auditable configuration.

## 4. Venue adapter contract

Each adapter implements capability discovery, authentication, instrument mapping, submit, cancel, order lookup, fill retrieval, balance/position retrieval where supported, rate-limit handling, health status, and reconciliation. Unsupported capabilities are declared rather than simulated. Credentials are least-privilege, environment-scoped, and never exposed to strategy or research processes.

Submission is idempotent where the venue supports client order IDs. If venue semantics do not guarantee idempotency, the adapter must use a durable submission record and reconciliation-based uncertainty handling. A timeout after a possible submission yields OMS `UNKNOWN`; no exposure-increasing retry occurs until the venue state is resolved or an authorized recovery procedure explicitly determines the safe action.

## 5. Market-specific constraints

Crypto, FX, equities, and commodities are independently enabled. Each market adapter supplies applicable lot size, tick size, minimum notional, trading session, order types, fees, funding/financing, settlement, and shorting/leverage capabilities. The Risk Engine rejects an order if a required rule is unknown, expired, or inconsistent. Market-specific behavior is implemented in adapter capability/rule data, not scattered conditional logic.

## 6. Adapter certification

Before paper or shadow use, an adapter passes schema/contract tests, precision and boundary tests, authentication/permission tests, rate-limit tests, timeout/duplicate tests, order-state mapping tests, reconciliation tests, and a documented failure-injection suite. Live eligibility additionally requires environment isolation, operational runbooks, venue-specific risk policy, owner approval, and successful paper/shadow evidence. Certification expires after material adapter, venue API, permission, or contract changes.
