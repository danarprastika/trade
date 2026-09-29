# Execution Gates

## G0 — Specification Integrity

Pass criteria: all documents listed in the manifest exist; every SHA-256 digest matches the exact packaged bytes; no unlisted document or legacy version is present; no duplicate authority or unresolved architecture choice remains; and the gate report records reviewer, commit, timestamp, command output, and pass/fail result. A mismatch is an automatic failure. G0 is documentation/package integrity only; repository scaffolding and executable contracts belong to G1.

## G1 — Domain Foundation

Pass criteria: canonical IDs, timestamps, money/quantity types, errors, envelopes, idempotency, configuration boundaries, migrations, and domain tests are implemented and verified.

## G2 — Market Data

Pass criteria: normalized instruments, feed health, freshness controls, provenance, and replayable ingestion are verified.

## G3 — Trading Core

Pass criteria: risk gate, OMS state machine, order invariants, halt behavior, ledger, and reconciliation tests pass.

## G4 — Research

Pass criteria: datasets are versioned, backtests are deterministic, artifacts are reproducible, and no research worker has financial authority.

## G5 — AI Company OS

Pass criteria: identity, model registry, decision ledger, tool permissions, and governance workflows operate with complete audit trails.

## G6 — Simulation / Shadow

Pass criteria: realistic execution simulation, shadow reconciliation, fault injection, and performance targets pass.

## G7 — Operator Interfaces

Pass criteria: web and Telegram actions use the same authorization path, privileged actions are audited, and UI cannot bypass server controls.

## G8 — Venue Adapters

Pass criteria: adapter contract tests, precision rules, rate limits, timeout handling, reconciliation, and venue-specific failure modes pass.

## G9 — Security / Operations

Pass criteria: security scans, secrets isolation, access review, observability, incident drills, backup restore, and recovery exercise pass.

## G10 — Production Readiness

Pass criteria: all prior gates pass, release evidence is complete, capacity targets pass, rollback is verified, and operational ownership is assigned.

## G11 — Live Activation

Pass criteria: G10 is passed; the account holder is legally eligible and identity/account authority is verified; the exact jurisdiction/account/venue/product/instrument/activity tuple has current approved eligibility evidence; live credentials are provisioned only in isolated live infrastructure with withdrawal/transfer permissions disabled; all account-, venue-, instrument-, and strategy-specific risk limits are configured and independently reviewed; no material reconciliation break, stale market data, unresolved UNKNOWN order, or active halt exists; required dual-control approvals come from two distinct authorized identities; owner authorization is recorded against the exact configuration/artifact digests; a canary activation runs in the narrowest permitted scope; abort thresholds and the responsible operator are confirmed; and the activation evidence is immutable. Failure of any condition leaves live mode disabled. This gate is not satisfied by documentation alone.

Each gate report must identify its evidence artifacts by immutable URI or repository path and content digest. A gate is PASS only when every listed criterion is satisfied; partial completion is FAIL, not a percentage. No gate may be marked passed by documentation alone when runtime evidence is required. Waivers are prohibited for financial invariants, authorization, live credential isolation, reconciliation, halt controls, and recovery objectives. Any noncritical exception requires a time-bounded risk acceptance by the owner and security/risk reviewers, a compensating control, expiry date, and linked remediation issue; it does not convert a failed mandatory criterion into PASS.
