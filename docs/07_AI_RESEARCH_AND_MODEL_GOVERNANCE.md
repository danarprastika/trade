# AI Research and Model Governance

## Model lifecycle

`REGISTERED -> EVALUATED -> VALIDATED -> APPROVED -> PAPER -> SHADOW -> PROMOTED -> MONITORED -> RETIRED`.

## Required model record

Every model has owner, version, training dataset fingerprint, code revision, feature specification, evaluation results, limitations, approval record, deployment scope, monitoring policy, and rollback artifact.

## Separation of duties

Research authors cannot self-approve a model for live use. Approval requires an independent reviewer and successful deterministic trading-system gates.

## Drift and degradation

Production monitoring covers input drift, output distribution drift, prediction stability, strategy performance, rejection rate, data freshness, and model-service health. A breached model threshold causes automatic pause of the affected strategy/model deployment.

## AI tool controls

AI agents use an explicit tool allowlist. Tools are classified read-only, state-changing, or financial. Financial tools can only be invoked through the Go command path and therefore pass normal authorization and risk controls.

## Prompt and artifact lineage

Prompts, model versions, tool calls, retrieved context identifiers, and generated decisions are recorded for material trading decisions. Sensitive credentials and raw secrets are never stored in model context or audit payloads.

## Human override

Operators can pause strategies and system components through authenticated control paths. Override actions are logged with actor, reason, scope, and timestamp.
