# Canonical Contracts

## Identifier format

All externally referenced identifiers use typed prefixes and lowercase Crockford Base32 payloads. Examples: `ord_`, `evt_`, `cmd_`, `str_`, `mdl_`, `run_`, `pos_`, `led_`.

## Money

Money is represented as `{currency, amount}` where `amount` is a base-10 decimal string. Currency is ISO 4217 for fiat and an explicit configured asset code for digital assets. No IEEE-754 floating point value may cross a financial contract.

## Quantity and price

Quantities and prices are decimal strings plus explicit scale metadata where the venue requires it. Venue precision is validated before submission.

## Time

All persisted timestamps are UTC RFC 3339 with nanosecond precision. Events carry `occurred_at`, `recorded_at`, and `source_timestamp` where applicable.

## Command envelope

Required fields: `command_id`, `command_type`, `schema_version`, `correlation_id`, `causation_id`, `actor_id`, `actor_type`, `environment`, `requested_at`, `payload`.

## Event envelope

Required fields: `event_id`, `event_type`, `schema_version`, `aggregate_type`, `aggregate_id`, `correlation_id`, `causation_id`, `producer_id`, `occurred_at`, `recorded_at`, `sequence`, `payload`.

## Idempotency

Every externally initiated mutating command requires an idempotency key scoped to actor, endpoint, and environment. Replays return the original command result. Database uniqueness constraints enforce idempotency; application memory is never sufficient.

## Error taxonomy

`VALIDATION_ERROR`, `AUTHENTICATION_ERROR`, `AUTHORIZATION_ERROR`, `CONFLICT`, `RISK_REJECTED`, `RATE_LIMITED`, `DEPENDENCY_UNAVAILABLE`, `TIMEOUT_UNKNOWN`, `DATA_STALE`, `RECONCILIATION_REQUIRED`, `INTERNAL_ERROR`.

## Compatibility

Consumers must tolerate additive fields. Breaking changes require a new schema version and dual-read/dual-write migration where persisted data is affected. Unknown enum values are treated as unsupported, not silently mapped.
