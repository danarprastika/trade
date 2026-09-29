# Implementation Handoff

## Phase 1 — Foundation

Create repository structure, Go module, Next.js application, Python workspace, contract package, PostgreSQL migrations, CI, linting, security scanning, and local development environment.

## Phase 2 — Domain core

Implement canonical types, command/event envelopes, identity, configuration, strategy state machine, deterministic risk rules, OMS, ledger, and audit records.

## Phase 3 — Persistence and eventing

Implement transactional repositories with sqlc, outbox, dispatcher, idempotency constraints, migrations, replay utilities, and reconciliation case storage.

## Phase 4 — Market and execution

Implement market-data normalization, freshness checks, venue adapter interfaces, simulated execution, paper/shadow execution, and reconciliation.

## Phase 5 — Research and AI governance

Implement dataset registry, backtest engine, experiment registry, model registry, model evaluation, tool allowlists, decision ledger, and promotion workflow.

## Phase 6 — Operator plane

Implement web control plane, audit views, risk dashboards, operational controls, and Telegram as a secondary authenticated interface using the same Go commands.

## Phase 7 — Production hardening

Execute security testing, performance testing, fault injection, backup restore, disaster recovery, observability validation, deployment rehearsal, and rollback verification.

## Phase 8 — Controlled activation

Pass G0–G10 first. Only then may the separately approved, isolated live environment be provisioned for the exact authorized scope. Apply reviewed account/venue parameters, execute the activation runbook, verify clean reconciliation, and complete G11 with two distinct authorized approvers. G11 is not implied by passing G0–G10.

## Definition of complete implementation

Implementation is complete only when every gate G0-G11 has evidence, every canonical contract has a consumer/provider test, every financial invariant has automated coverage, recovery objectives have been demonstrated, and the production release can be reproduced from immutable source and artifact digests.
