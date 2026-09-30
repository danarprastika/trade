"""live_activation_guard -- fail-closed evaluator for execution gate G11.

Reads a proposed live configuration plus the jurisdiction eligibility registry and
returns DENY unless EVERY binding G11 precondition from
``11_EXECUTION_GATES.md`` is satisfied.

This module is stdlib-only and targets Python 3.14.6
(``00_README.md`` toolchain lifecycle; ``23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md``
section 7 requires exact pinning and rejects end-of-life runtimes).

DESIGN CONTRACT: FAIL CLOSED.
    Missing, expired, contradictory, or unparseable input yields DENY. There is no
    default-permit path, no partial-pass path, and no "unknown means probably fine"
    path.

    11_EXECUTION_GATES.md: "A gate is PASS only when every listed criterion is
    satisfied; partial completion is FAIL, not a percentage."
    18_GOVERNANCE_DATA_AND_COMPLIANCE.md section 5: "Unknown, expired, contradictory,
    or revoked eligibility records fail closed."
    25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_PROFILE.md section 3
    invariant 7: "Live capability is disabled by default and cannot be activated
    without exact-scope legal eligibility, verified adult account-holder authority,
    dual approval, and passing release gates. No control may be bypassed or inferred
    from device location, IP, or a successful API connection."

G11 PRECONDITIONS ENFORCED (each maps to a named method returning a Check):
    1.  g10_passed
    2.  account_holder_adult_and_eligible
    3.  eligibility_current_exact_scope
    4.  dual_control_two_distinct_approvers
    5.  no_unresolved_material_reconciliation_break
    6.  no_unknown_order
    7.  no_active_halt
    8.  market_data_fresh
    9.  risk_limits_reviewed
    10. live_credentials_isolated_withdrawal_disabled
    11. owner_authorization_against_exact_digests
    12. canary_scope_and_abort_criteria

NON-WAIVABLE (11_EXECUTION_GATES.md): "Waivers are prohibited for financial
invariants, authorization, live credential isolation, reconciliation, halt
controls, and recovery objectives." Every check below is non-waivable; this module
has no waiver parameter by design.
"""

from __future__ import annotations

import json
import sys
from collections.abc import Iterable, Mapping, Sequence
from dataclasses import dataclass, field
from datetime import date, datetime, timezone
from pathlib import Path
from typing import Any

# --------------------------------------------------------------------------
# Binding numeric defaults. Every value below is cited to its blueprint sentence.
# These are NOT tunable: 23 section 6 states "These values are platform defaults,
# not statements of legal or venue requirements. A stricter rule always prevails. A
# relaxed value requires a reviewed ADR, risk assessment, and updated tests."
# --------------------------------------------------------------------------

#: 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md section 5: "Eligibility records
#: expire after at most 30 days unless a stricter interval is required."
#: section 6 table: "Eligibility record maximum age | 30 days".
#: 18_GOVERNANCE_DATA_AND_COMPLIANCE.md section 5.1: "An eligibility record is
#: revalidated at least every 30 days".
ELIGIBILITY_MAX_AGE_DAYS = 30

#: 06_SECURITY_AND_ACCESS_CONTROL.md section 2: "Re-authentication freshness is at
#: most 5 minutes for live activation and other high-impact actions."
#: 21_ZERO_TRUST_SSO_AND_AUTHORIZATION.md section 3: "5 minutes for high-impact
#: actions".
STEP_UP_FRESHNESS_MAX_MINUTES = 5

#: 21_ZERO_TRUST_SSO_AND_AUTHORIZATION.md section 6: "Approval expires after 24
#: hours and must be renewed." 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md section
#: 6 table: "High-impact approval validity | 24 hours; exact diff only".
APPROVAL_MAX_VALIDITY_HOURS = 24

#: Minimum age of majority used by this guard. 23 section 5 requires a "legally
#: eligible adult account holder"; 12_DECISION_REGISTER.md ADR-024 requires "an
#: eligible adult account holder" with "No guardian or account-sharing workaround".
#: The blueprints deliberately do not publish a single universal numeric minimum
#: age: 18_GOVERNANCE_DATA_AND_COMPLIANCE.md section 5.1 refers to "applicable
#: minimum-age requirements" and 20_FINAL_AUDIT_CLOSURE_AND_ACCEPTANCE.md section 3
#: lists under-age declarations as a deny condition rather than a fixed number. 18
#: is therefore used as the concrete adult threshold for the guard's arithmetic, and
#: a stricter jurisdiction-specific minimum age supplied in the record is enforced
#: as a stricter rule (23 section 6: "A stricter rule always prevails").
MINIMUM_ADULT_AGE = 18

#: 06_SECURITY_AND_ACCESS_CONTROL.md section 2: sessions idle-expire after 15
#: minutes for privileged operators; 8 hours absolute.
#: 21_ZERO_TRUST_SSO_AND_AUTHORIZATION.md section 3 table.
PRIVILEGED_IDLE_TIMEOUT_MINUTES = 15

#: 11_EXECUTION_GATES.md G11: "no material reconciliation break, stale market data,
#: unresolved UNKNOWN order, or active halt exists". Each of these is a hard zero.
MAX_UNRESOLVED_MATERIAL_BREAKS = 0
MAX_UNRESOLVED_UNKNOWN_ORDERS = 0
MAX_ACTIVE_HALTS = 0

#: Approved decision values for an eligibility record. 18 section 5 uses a
#: POSITIVE registry: "The platform uses a positive eligibility registry." Only
#: APPROVED enables a scope.
APPROVED_DECISIONS = frozenset({"APPROVED"})

#: Deny-by-default decisions and malformed states, per 18 section 5 fail-closed.
DENY_DECISIONS = frozenset(
    {"DENIED", "REVOKED", "PENDING", "EXPIRED", "UNKNOWN", "REJECTED", "SUSPENDED"}
)

SHA256_HEX_LENGTH = 64


class GuardInputError(Exception):
    """Raised when an input document cannot be read or parsed at all.

    11_EXECUTION_GATES.md: "Missing, stale, unverifiable, or contradictory evidence
    is a release failure." An unparseable document is a DENY, not an exception the
    caller may swallow.
    """


# --------------------------------------------------------------------------
# Result types
# --------------------------------------------------------------------------


@dataclass(frozen=True, slots=True)
class Check:
    """One G11 precondition evaluation.

    ``ok`` is True only when the precondition is positively satisfied by
    well-formed, current, non-contradictory evidence. Anything else is False.
    """

    precondition: str
    ok: bool
    detail: str
    citation: str
    non_waivable: bool = True


@dataclass(frozen=True, slots=True)
class GuardResult:
    """The overall G11 decision. ``allowed`` is True only for ALLOW."""

    decision: str  # "ALLOW" or "DENY"
    checks: tuple[Check, ...] = field(default_factory=tuple)
    scope: str = ""

    @property
    def allowed(self) -> bool:
        return self.decision == "ALLOW"

    @property
    def denied_checks(self) -> tuple[Check, ...]:
        return tuple(c for c in self.checks if not c.ok)

    def to_dict(self) -> dict[str, Any]:
        return {
            "decision": self.decision,
            "scope": self.scope,
            "allowed": self.allowed,
            "checks": [
                {
                    "precondition": c.precondition,
                    "result": "PASS" if c.ok else "FAIL",
                    "detail": c.detail,
                    "citation": c.citation,
                    "non_waivable": c.non_waivable,
                }
                for c in self.checks
            ],
        }

    def render(self) -> str:
        lines = [
            f"G11 DECISION: {self.decision}",
            f"scope: {self.scope or '<unspecified>'}",
            "",
        ]
        for c in self.checks:
            mark = "PASS" if c.ok else "FAIL"
            lines.append(f"  [{mark}] {c.precondition}: {c.detail}")
        if self.denied_checks:
            lines.append("")
            lines.append("DENY basis (11_EXECUTION_GATES.md: partial completion is FAIL):")
            for c in self.denied_checks:
                lines.append(f"  - {c.precondition} -> {c.detail}")
        return "\n".join(lines)


# --------------------------------------------------------------------------
# Small parsing helpers. Every one of these fails closed.
# --------------------------------------------------------------------------


def _require_mapping(value: Any, where: str) -> Mapping[str, Any]:
    if not isinstance(value, Mapping):
        raise GuardInputError(f"{where}: expected an object, got {type(value).__name__}")
    return value


def _get(mapping: Mapping[str, Any], key: str, where: str) -> Any:
    if key not in mapping:
        raise GuardInputError(f"{where}: required field '{key}' is missing")
    return mapping[key]


def _get_bool(mapping: Mapping[str, Any], key: str, where: str) -> bool:
    value = _get(mapping, key, where)
    if not isinstance(value, bool):
        raise GuardInputError(
            f"{where}: field '{key}' must be a boolean, got {type(value).__name__}"
        )
    return value


def _get_str(mapping: Mapping[str, Any], key: str, where: str) -> str:
    value = _get(mapping, key, where)
    if not isinstance(value, str):
        raise GuardInputError(
            f"{where}: field '{key}' must be a string, got {type(value).__name__}"
        )
    stripped = value.strip()
    if not stripped:
        raise GuardInputError(f"{where}: field '{key}' must not be empty")
    return stripped


def _get_int(mapping: Mapping[str, Any], key: str, where: str) -> int:
    value = _get(mapping, key, where)
    # bool is a subclass of int; an explicit reject avoids `True` counting as 1.
    if isinstance(value, bool) or not isinstance(value, int):
        raise GuardInputError(
            f"{where}: field '{key}' must be an integer, got {type(value).__name__}"
        )
    return value


def _get_str_list(mapping: Mapping[str, Any], key: str, where: str) -> list[str]:
    value = _get(mapping, key, where)
    if isinstance(value, str) or not isinstance(value, Sequence):
        raise GuardInputError(
            f"{where}: field '{key}' must be a list of strings, got {type(value).__name__}"
        )
    out: list[str] = []
    for i, item in enumerate(value):
        if not isinstance(item, str) or not item.strip():
            raise GuardInputError(f"{where}: field '{key}[{i}]' must be a non-empty string")
        out.append(item.strip())
    return out


def _parse_date(value: str, where: str) -> date:
    try:
        return date.fromisoformat(value)
    except ValueError as exc:
        raise GuardInputError(f"{where}: '{value}' is not a valid ISO-8601 date") from exc


def _is_sha256_digest(value: str) -> bool:
    if not value.startswith("sha256:"):
        return False
    hexpart = value[len("sha256:") :]
    return len(hexpart) == SHA256_HEX_LENGTH and all(
        c in "0123456789abcdef" for c in hexpart.lower()
    )


def _today(now: datetime) -> date:
    return now.astimezone(timezone.utc).date()


# --------------------------------------------------------------------------
# The guard
# --------------------------------------------------------------------------


class LiveActivationGuard:
    """Fail-closed evaluator for the G11 live-activation gate.

    Parameters
    ----------
    eligibility_registry:
        Sequence of eligibility records conforming to
        ``infra/governance/eligibility.schema.json``, or a single record mapping.
    now:
        Evaluation instant. Injected so tests are deterministic. Callers MUST pass
        the real current time in production; the guard never reads the wall clock
        itself when ``now`` is supplied.
    """

    def __init__(
        self,
        eligibility_registry: Iterable[Mapping[str, Any]] | Mapping[str, Any],
        *,
        now: datetime | None = None,
    ) -> None:
        self._records = self._normalise_registry(eligibility_registry)
        self._now = now or datetime.now(timezone.utc)
        if self._now.tzinfo is None:
            raise GuardInputError("now: must be timezone-aware")

    # -- registry ---------------------------------------------------------

    @staticmethod
    def _normalise_registry(
        registry: Iterable[Mapping[str, Any]] | Mapping[str, Any],
    ) -> tuple[Mapping[str, Any], ...]:
        if isinstance(registry, Mapping):
            candidates: Sequence[Any] = [registry]
        elif isinstance(registry, (str, bytes)):
            raise GuardInputError("eligibility_registry: must be a mapping or a sequence")
        else:
            try:
                candidates = list(registry)
            except TypeError as exc:
                raise GuardInputError(
                    "eligibility_registry: must be iterable of records"
                ) from exc
        if not candidates:
            # 18 section 5: the registry is a POSITIVE list. An empty registry
            # therefore enables nothing.
            return ()
        for i, rec in enumerate(candidates):
            if not isinstance(rec, Mapping):
                raise GuardInputError(
                    f"eligibility_registry[{i}]: expected an object, got {type(rec).__name__}"
                )
        return tuple(candidates)

    # -- public API --------------------------------------------------------

    def evaluate(self, proposed_live_config: Mapping[str, Any]) -> GuardResult:
        """Evaluate a proposed live configuration against G11.

        Returns ALLOW only when every precondition passes. Any exception raised
        while reading the configuration is converted into a DENY result rather
        than propagating, so a caller cannot accidentally treat a parse failure as
        anything other than a deny.
        """
        try:
            config = _require_mapping(proposed_live_config, "proposed_live_config")
            scope = self._scope_of(config)
            checks = (
                self._c01_g10_passed(config),
                self._c02_account_holder_adult_and_eligible(config),
                self._c03_eligibility_current_exact_scope(config, scope),
                self._c04_dual_control_two_distinct_approvers(config),
                self._c05_no_unresolved_material_reconciliation_break(config),
                self._c06_no_unknown_order(config),
                self._c07_no_active_halt(config),
                self._c08_market_data_fresh(config),
                self._c09_risk_limits_reviewed(config),
                self._c10_live_credentials_isolated_withdrawal_disabled(config),
                self._c11_owner_authorization_against_exact_digests(config),
                self._c12_canary_scope_and_abort_criteria(config),
            )
        except GuardInputError as exc:
            return GuardResult(
                decision="DENY",
                checks=(
                    Check(
                        precondition="input_integrity",
                        ok=False,
                        detail=(
                            f"proposed live configuration could not be read or parsed: {exc}. "
                            "Missing, unverifiable, or contradictory evidence is a release "
                            "failure (11_EXECUTION_GATES.md section 8)."
                        ),
                        citation=(
                            "11_EXECUTION_GATES.md: 'Missing, stale, unverifiable, or "
                            "contradictory evidence is a release failure.'; fail-closed per "
                            "18_GOVERNANCE_DATA_AND_COMPLIANCE.md section 5."
                        ),
                    ),
                ),
            )
        decision = "ALLOW" if all(c.ok for c in checks) else "DENY"
        return GuardResult(decision=decision, checks=checks, scope=scope)

    # -- scope -------------------------------------------------------------

    #: The 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md section 5 evaluation tuple.
    SCOPE_FIELDS: tuple[str, ...] = (
        "declared_residence",
        "account_holder",
        "legal_entity",
        "venue",
        "account_type",
        "market_class",
        "instrument",
        "product",
        "activity",
        "api_permission",
    )

    def _scope_of(self, config: Mapping[str, Any]) -> str:
        parts: list[str] = []
        for field_name in self.SCOPE_FIELDS:
            if field_name in config and isinstance(config[field_name], (str, Mapping)):
                value = config[field_name]
                if isinstance(value, Mapping):
                    ident = value.get("account_holder_id", "")
                else:
                    ident = value
                if isinstance(ident, str) and ident.strip():
                    parts.append(f"{field_name}={ident.strip()}")
        return "+".join(parts)

    # -- individual preconditions -----------------------------------------

    def _c01_g10_passed(self, config: Mapping[str, Any]) -> Check:
        precondition = "g10_passed"
        citation = (
            '11_EXECUTION_GATES.md G11: "G10 is passed". 11_EXECUTION_GATES.md: "A gate is '
            'PASS only when every listed criterion is satisfied; partial completion is FAIL, not '
            'a percentage." 13_IMPLEMENTATION_HANDOFF.md: "G11 is not implied by passing '
            'G0-G10."'
        )
        gates = _get(config, "gate_reports", "proposed_live_config")
        if isinstance(gates, Mapping) or isinstance(gates, str) or not isinstance(gates, Sequence):
            raise GuardInputError(
                "proposed_live_config.gate_reports: expected a list of gate report objects"
            )
        found: Mapping[str, Any] | None = None
        for i, report in enumerate(gates):
            report_map = _require_mapping(report, f"proposed_live_config.gate_reports[{i}]")
            if _get_str(report_map, "gate_id", f"gate_reports[{i}]") == "G10":
                found = report_map
                break
        if found is None:
            return Check(
                precondition, False, "no G10 gate report present in the evidence set", citation
            )
        result = _get_str(found, "result", "gate_reports[G10]").upper()
        if result != "PASS":
            return Check(
                precondition,
                False,
                f"G10 gate report result is {result!r}, not PASS",
                citation,
            )
        # A gate report without a reviewer or an evidence reference is not
        # verifiable. 25 section 8 item 9 requires evidence references and
        # reviewer identities.
        if not _get_str(found, "evidence_uri", "gate_reports[G10]"):
            return Check(
                precondition, False, "G10 gate report has no evidence_uri", citation
            )
        if not _get_str(found, "reviewed_by", "gate_reports[G10]"):
            return Check(precondition, False, "G10 gate report has no reviewed_by identity", citation)
        return Check(precondition, True, "G10 gate report is PASS with evidence and reviewer", citation)

    def _c02_account_holder_adult_and_eligible(self, config: Mapping[str, Any]) -> Check:
        precondition = "account_holder_adult_and_eligible"
        citation = (
            '11_EXECUTION_GATES.md G11: "the account holder is legally eligible and '
            'identity/account authority is verified". 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md '
            'section 5: "Live capability is restricted to a legally eligible adult account holder '
            'with verified identity and account authority. The system does not support shared '
            'credentials, false age/residence declarations, or bypasses." 18_'
            'GOVERNANCE_DATA_AND_COMPLIANCE.md section 5.1: the platform "must refuse live '
            'activation for a person who is not legally eligible ... including applicable '
            'minimum-age requirements. It must not support account sharing, false declarations, '
            'or bypass of identity/age checks."'
        )
        holder = _require_mapping(
            _get(config, "account_holder", "proposed_live_config"),
            "proposed_live_config.account_holder",
        )
        where = "proposed_live_config.account_holder"

        if not _get_bool(holder, "legal_capacity_confirmed", where):
            return Check(
                precondition, False, "legal capacity to hold and operate the account is not confirmed", citation
            )
        if not _get_bool(holder, "adult_confirmed", where):
            return Check(precondition, False, "account holder is not confirmed to be an adult", citation)
        if not _get_bool(holder, "identity_verified", where):
            return Check(precondition, False, "account holder identity is not verified", citation)
        if not _get_bool(holder, "account_authority_verified", where):
            return Check(
                precondition, False, "account holder authority to operate the account is not verified", citation
            )
        if not _get_bool(holder, "age_verified", where):
            return Check(
                precondition,
                False,
                "age is not verified against an authoritative source; 18 section 5.1 prohibits "
                "self-declared age and bypass of identity/age checks",
                citation,
            )

        # Concrete age arithmetic. Under-18 is always ineligible; a declared
        # higher jurisdiction-specific minimum age is also enforced, because
        # 23 section 6 states "A stricter rule always prevails."
        declared_age = _get(holder, "age", where)
        if isinstance(declared_age, bool) or not isinstance(declared_age, int):
            raise GuardInputError(
                f"{where}.age: must be an integer age, got {type(declared_age).__name__}"
            )
        minimum_age = _get_int(holder, "applicable_minimum_age", where)
        if minimum_age < MINIMUM_ADULT_AGE:
            minimum_age = MINIMUM_ADULT_AGE
        if declared_age < minimum_age:
            return Check(
                precondition,
                False,
                f"account holder declares age {declared_age}, below the required minimum age "
                f"{minimum_age}; 18 section 5.1 requires refusal of live activation for a person "
                f"who is not legally eligible, including applicable minimum-age requirements",
                citation,
            )
        determined_by = _get_str(holder, "age_determined_by", where)
        if determined_by.lower() in {"operator-assertion", "self-declared", "self-declared", "user-input"}:
            return Check(
                precondition,
                False,
                f"age was determined by {determined_by!r}, which is not an authoritative source; "
                "18 section 5.1 prohibits false declarations and bypass of age checks",
                citation,
            )
        return Check(
            precondition,
            True,
            f"account holder confirmed adult (age {declared_age} >= minimum {minimum_age}), "
            "identity-verified, account-authority-verified, and legally capable",
            citation,
        )

    def _c03_eligibility_current_exact_scope(
        self, config: Mapping[str, Any], scope: str
    ) -> Check:
        precondition = "eligibility_current_exact_scope"
        citation = (
            '11_EXECUTION_GATES.md G11: "the exact jurisdiction/account/venue/product/instrument/'
            'activity tuple has current approved eligibility evidence". 23_ARCHITECTURE_AND_'
            'COMPLIANCE_DECISIONS.md section 5: eligibility is evaluated over the tuple '
            '"declared_residence + account_holder + legal_entity + venue + account_type + '
            'market_class + instrument + product + activity + API_permission + effective_date"; '
            '"Eligibility records expire after at most 30 days unless a stricter interval is '
            'required." 18_GOVERNANCE_DATA_AND_COMPLIANCE.md section 5: "Unknown, expired, '
            'contradictory, or revoked eligibility records fail closed. Scope expansion requires a '
            'new approval. ... No live credentials are provisioned before eligibility approval."'
        )
        if not self._records:
            return Check(
                precondition,
                False,
                "the eligibility registry contains no records; the registry is a POSITIVE list, so "
                "an empty registry approves nothing (18 section 5)",
                citation,
            )

        # Exact-scope match on every tuple field. A record covering a wider scope
        # does not authorise a narrower one by inference: 18 section 5 states
        # "Scope expansion requires a new approval", and by symmetry a broader
        # approval cannot be assumed to cover this exact tuple.
        for field_name in self.SCOPE_FIELDS:
            if field_name not in config:
                raise GuardInputError(
                    f"proposed_live_config: scope field '{field_name}' is missing; the 23 section 5 "
                    "evaluation tuple must be stated explicitly for live activation"
                )

        today = _today(self._now)
        max_age = ELIGIBILITY_MAX_AGE_DAYS

        matched: list[Mapping[str, Any]] = []
        rejections: list[str] = []

        for record in self._records:
            where = "eligibility_registry record"
            try:
                if not self._record_matches_scope(record, config):
                    continue
            except GuardInputError as exc:
                rejections.append(str(exc))
                continue
            matched.append(record)

        if not matched:
            detail = (
                "no eligibility record matches the exact "
                f"jurisdiction/account/venue/product/instrument/activity scope ({scope or 'unstated'})"
            )
            if rejections:
                detail += "; unusable records: " + "; ".join(rejections)
            return Check(precondition, False, detail, citation)

        # Contradiction is a deny even when other records approve.
        decisions = {_get_str(r, "decision", "record").upper() for r in matched}
        if decisions & DENY_DECISIONS:
            return Check(
                precondition,
                False,
                f"contradictory eligibility evidence: the exact scope has records with denying "
                f"decisions {sorted(decisions & DENY_DECISIONS)}; 18 section 5 requires fail-closed "
                "handling of contradictory records",
                citation,
            )
        approved = [r for r in matched if _get_str(r, "decision", "record").upper() in APPROVED_DECISIONS]
        if not approved:
            return Check(
                precondition,
                False,
                f"no APPROVED eligibility record for the exact scope; decisions present: {sorted(decisions)}",
                citation,
            )

        # Freshness: expiry, next_review, and the 30-day maximum age.
        stale: list[str] = []
        for record in approved:
            record_id = _get_str(record, "record_id", "eligibility record")
            expiry = _parse_date(_get_str(record, "expiry", f"record {record_id}"), f"record {record_id}.expiry")
            next_review = _parse_date(
                _get_str(record, "next_review", f"record {record_id}"), f"record {record_id}.next_review"
            )
            effective = _parse_date(
                _get_str(record, "effective_date", f"record {record_id}"),
                f"record {record_id}.effective_date",
            )
            if expiry < today:
                stale.append(f"{record_id}: expired on {expiry.isoformat()}")
                continue
            if next_review < today:
                stale.append(f"{record_id}: next_review {next_review.isoformat()} has passed")
                continue
            if effective > today:
                stale.append(f"{record_id}: not yet effective (effective_date {effective.isoformat()})")
                continue
            age_days = _get_int(record, "record_age_days", f"record {record_id}")
            if age_days < 0:
                stale.append(f"{record_id}: record_age_days is negative ({age_days})")
                continue
            if age_days > max_age:
                stale.append(
                    f"{record_id}: record age {age_days}d exceeds the {max_age}d maximum "
                    "(23 section 5 and section 6)"
                )
                continue
            # The 72-hour official-source refresh tolerance.
            hours_since_refresh = _get_int(
                record, "hours_since_official_source_refresh", f"record {record_id}"
            )
            if hours_since_refresh > 72:
                stale.append(
                    f"{record_id}: official source not refreshed for {hours_since_refresh}h, "
                    "exceeding the 72h tolerance; the affected scope is blocked "
                    "(23 section 6, 18 section 5.1)"
                )
                continue
            if not _get_str(record, "reviewer", f"record {record_id}"):
                stale.append(f"{record_id}: no responsible reviewer recorded")
                continue
            if not _get_str(record, "source_authority", f"record {record_id}"):
                stale.append(f"{record_id}: no source authority recorded")
                continue
            if not _get_str(record, "rule_citation", f"record {record_id}"):
                stale.append(f"{record_id}: no rule citation recorded")
                continue
            if not _get_str(record, "evidence_source", f"record {record_id}"):
                stale.append(f"{record_id}: no evidence source recorded")
                continue
            return Check(
                precondition,
                True,
                f"current APPROVED eligibility evidence for the exact scope "
                f"(record {record_id}, age {age_days}d <= {max_age}d, expiry {expiry.isoformat()}, "
                f"source refreshed {hours_since_refresh}h ago)",
                citation,
            )

        return Check(
            precondition,
            False,
            "no current APPROVED eligibility record; " + "; ".join(stale),
            citation,
        )

    @staticmethod
    def _record_matches_scope(record: Mapping[str, Any], config: Mapping[str, Any]) -> bool:
        for field_name in LiveActivationGuard.SCOPE_FIELDS:
            if field_name == "account_holder":
                # Compared by account_holder_id rather than the whole object.
                record_holder = record.get("account_holder")
                config_holder = config.get("account_holder")
                if isinstance(record_holder, Mapping) and isinstance(config_holder, Mapping):
                    left = str(record_holder.get("account_holder_id", "")).strip()
                    right = str(config_holder.get("account_holder_id", "")).strip()
                else:
                    left = str(record_holder or "").strip()
                    right = str(config_holder or "").strip()
            else:
                left = str(record.get(field_name, "")).strip()
                right = str(config.get(field_name, "")).strip()
            if left != right:
                return False
        return True

    def _c04_dual_control_two_distinct_approvers(self, config: Mapping[str, Any]) -> Check:
        precondition = "dual_control_two_distinct_approvers"
        citation = (
            '11_EXECUTION_GATES.md G11: "required dual-control approvals come from two distinct '
            'authorized identities". 06_SECURITY_AND_ACCESS_CONTROL.md section 3: "The initiating '
            'actor and approver must be distinct identities." 21_ZERO_TRUST_SSO_AND_AUTHORIZATION.md '
            'section 6: "Same identity cannot satisfy both approvals; any diff invalidates approval. '
            'Approval expires after 24 hours and must be renewed."'
        )
        dual = _require_mapping(
            _get(config, "dual_control", "proposed_live_config"), "proposed_live_config.dual_control"
        )
        where = "proposed_live_config.dual_control"
        requester = _get_str(dual, "requester_identity", where)
        approver = _get_str(dual, "approver_identity", where)
        if requester == approver:
            return Check(
                precondition,
                False,
                f"requester and approver are the same identity ({requester!r}); 06 section 3 requires "
                "distinct identities and 11_EXECUTION_GATES.md prohibits self-approval",
                citation,
            )
        if not _get_bool(dual, "exact_diff_reviewed", where):
            return Check(
                precondition,
                False,
                "approval was not recorded against the exact reviewed diff; 21 section 6 states any "
                "change to the diff invalidates approval",
                citation,
            )
        validity_hours = _get_int(dual, "approval_valid_hours", where)
        if validity_hours <= 0 or validity_hours > APPROVAL_MAX_VALIDITY_HOURS:
            return Check(
                precondition,
                False,
                f"approval validity {validity_hours}h is outside the permitted 24h maximum "
                "(23 section 6: 'High-impact approval validity | 24 hours; exact diff only')",
                citation,
            )
        freshness = _get_int(dual, "step_up_freshness_minutes", where)
        if freshness < 0 or freshness > STEP_UP_FRESHNESS_MAX_MINUTES:
            return Check(
                precondition,
                False,
                f"step-up freshness {freshness}m exceeds the {STEP_UP_FRESHNESS_MAX_MINUTES}m maximum "
                "required for live activation (06 section 2)",
                citation,
            )
        expires_at = _get_str(dual, "approval_expires_at", where)
        try:
            expiry_dt = datetime.fromisoformat(expires_at)
        except ValueError as exc:
            raise GuardInputError(f"{where}.approval_expires_at: not a valid ISO-8601 timestamp") from exc
        if expiry_dt.tzinfo is None:
            raise GuardInputError(f"{where}.approval_expires_at: must be timezone-aware")
        if expiry_dt <= self._now:
            return Check(
                precondition,
                False,
                f"approval expired at {expires_at}; 21 section 6 requires renewal after 24 hours",
                citation,
            )
        return Check(
            precondition,
            True,
            f"dual control satisfied by two distinct identities ({requester} requested, {approver} "
            f"approved), exact diff reviewed, step-up fresh ({freshness}m <= {STEP_UP_FRESHNESS_MAX_MINUTES}m)",
            citation,
        )

    def _c05_no_unresolved_material_reconciliation_break(self, config: Mapping[str, Any]) -> Check:
        precondition = "no_unresolved_material_reconciliation_break"
        citation = (
            '11_EXECUTION_GATES.md G11: "no material reconciliation break ... exists". 05_'
            'PERSISTENCE_EVENTING_RECONCILIATION.md section Reconciliation: "A material unresolved '
            'break blocks affected risk-increasing actions." 10_OPERATIONS_AND_DISASTER_RECOVERY.md: '
            're-enable requires "order/fill/balance/position reconciliation completed or explicitly '
            'dispositioned".'
        )
        breaks = _get_int(
            config, "unresolved_material_reconciliation_breaks", "proposed_live_config"
        )
        if breaks > MAX_UNRESOLVED_MATERIAL_BREAKS:
            return Check(
                precondition,
                False,
                f"{breaks} material reconciliation break(s) remain unresolved",
                citation,
            )
        if not _get_bool(config, "reconciliation_completed_or_dispositioned", "proposed_live_config"):
            return Check(
                precondition,
                False,
                "reconciliation has neither completed nor been explicitly dispositioned; 10 "
                "OPERATIONS_AND_DISASTER_RECOVERY.md requires one of the two before re-enable",
                citation,
            )
        return Check(
            precondition, True, "no material reconciliation break is unresolved", citation
        )

    def _c06_no_unknown_order(self, config: Mapping[str, Any]) -> Check:
        precondition = "no_unknown_order"
        citation = (
            '11_EXECUTION_GATES.md G11: no "unresolved UNKNOWN order" may exist. 04_TRADING_DOMAIN_'
            'AND_RISK.md section Order lifecycle: "UNKNOWN is mandatory when a submission timeout '
            'prevents determination of venue outcome. The system reconciles before any retry that '
            'could duplicate exposure." 10_OPERATIONS_AND_DISASTER_RECOVERY.md: "the system must not '
            'retry an exposure-increasing command."'
        )
        unknown = _get_int(config, "unresolved_unknown_orders", "proposed_live_config")
        if unknown > MAX_UNRESOLVED_UNKNOWN_ORDERS:
            return Check(
                precondition,
                False,
                f"{unknown} order(s) remain in the OMS UNKNOWN state; retrying before resolution "
                "could duplicate exposure",
                citation,
            )
        return Check(precondition, True, "no order is in the UNKNOWN state", citation)

    def _c07_no_active_halt(self, config: Mapping[str, Any]) -> Check:
        precondition = "no_active_halt"
        citation = (
            '11_EXECUTION_GATES.md G11: no "active halt" may exist. 04_TRADING_DOMAIN_AND_RISK.md '
            'section Halt hierarchy: "SYSTEM_HALT > VENUE_HALT > MARKET_HALT > STRATEGY_HALT > '
            'ACCOUNT_HALT." 17_CONFIGURATION_AND_RISK_POLICY.md section 2: an active halt means '
            'reject. 25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_PROFILE.md section 3 '
            'invariant 9: "A halt is monotonic".'
        )
        halts = _get_int(config, "active_halts", "proposed_live_config")
        if halts > MAX_ACTIVE_HALTS:
            return Check(
                precondition,
                False,
                f"{halts} halt(s) are active; clearing a halt requires the owning authority, a "
                "recorded reason, health checks, reconciliation status, and two-person approval "
                "(17 section 5)",
                citation,
            )
        return Check(precondition, True, "no halt is active", citation)

    def _c08_market_data_fresh(self, config: Mapping[str, Any]) -> Check:
        precondition = "market_data_fresh"
        citation = (
            '11_EXECUTION_GATES.md G11: no "stale market data" may exist. 16_MARKET_DATA_AND_VENUE_'
            'ADAPTERS.md section 2: "Market data is considered stale when its age exceeds the '
            'instrument/market-specific freshness threshold configured in the environment policy. ... '
            'Risk-increasing commands are rejected when required inputs are stale or degraded. The '
            'platform never substitutes a cached value without labeling its age and source."'
        )
        if _get_bool(config, "market_data_stale", "proposed_live_config"):
            return Check(
                precondition,
                False,
                "required market data is stale or degraded; risk-increasing commands must be rejected "
                "and no cached value may be substituted",
                citation,
            )
        breaches = _get(config, "market_data_freshness_breaches", "proposed_live_config")
        if isinstance(breaches, str) or not isinstance(breaches, Sequence):
            raise GuardInputError(
                "proposed_live_config.market_data_freshness_breaches: expected a list of "
                "instrument identifiers"
            )
        for i, item in enumerate(breaches):
            if not isinstance(item, str) or not item.strip():
                raise GuardInputError(
                    f"proposed_live_config.market_data_freshness_breaches[{i}]: must be a non-empty "
                    "instrument identifier"
                )
        if breaches:
            return Check(
                precondition,
                False,
                f"market-data freshness limit breached for: {', '.join(str(b) for b in breaches)}",
                citation,
            )
        return Check(precondition, True, "required market data is within freshness limits", citation)

    def _c09_risk_limits_reviewed(self, config: Mapping[str, Any]) -> Check:
        precondition = "risk_limits_reviewed"
        citation = (
            '11_EXECUTION_GATES.md G11: "all account-, venue-, instrument-, and strategy-specific '
            'risk limits are configured and independently reviewed". 17_CONFIGURATION_AND_RISK_'
            'POLICY.md section 3: "The platform supplies no universal live numeric trading limits: '
            'the owner must configure values appropriate to the account, venue, jurisdiction, and '
            'risk mandate. Until the complete policy is validated and approved, live activation is '
            'blocked. This is a closed fail-safe rule, not an unresolved design choice."'
        )
        if not _get_bool(config, "risk_limits_reviewed", "proposed_live_config"):
            return Check(
                precondition, False, "account/venue/instrument/strategy risk limits are not reviewed", citation
            )
        policy = _require_mapping(
            _get(config, "risk_policy", "proposed_live_config"), "proposed_live_config.risk_policy"
        )
        where = "proposed_live_config.risk_policy"
        required_dimensions = (
            "permitted_markets",
            "permitted_venues",
            "permitted_instruments",
            "maximum_order_notional",
            "maximum_gross_exposure",
            "maximum_net_exposure",
            "maximum_leverage",
            "loss_threshold",
            "drawdown_threshold",
            "market_data_freshness_limit",
            "order_rate_limit",
            "cancel_rate_limit",
            "operating_mode",
        )
        missing = [d for d in required_dimensions if d not in policy]
        if missing:
            return Check(
                precondition,
                False,
                f"risk policy is missing required dimensions: {', '.join(sorted(missing))}; 17 section 3 "
                "requires each to be explicitly defined for its account/strategy/instrument/environment "
                "scope",
                citation,
            )
        unvalued = [
            d
            for d in required_dimensions
            if policy.get(d) is None or (isinstance(policy.get(d), (str, list)) and not policy.get(d))
        ]
        if unvalued:
            return Check(
                precondition,
                False,
                f"risk policy dimensions present but unvalued: {', '.join(sorted(unvalued))}; a missing "
                "limit is a deny condition (17 section 2: 'Missing limits ... means reject')",
                citation,
            )
        reviewer = _get_str(policy, "independent_reviewer", where)
        requester = _get_str(policy, "configured_by", where)
        if reviewer == requester:
            return Check(
                precondition,
                False,
                f"risk policy was configured and reviewed by the same identity ({requester!r}); G11 "
                "requires independent review",
                citation,
            )
        return Check(
            precondition,
            True,
            f"all required risk policy dimensions are valued, independently reviewed by {reviewer} "
            f"(configured by {requester})",
            citation,
        )

    def _c10_live_credentials_isolated_withdrawal_disabled(self, config: Mapping[str, Any]) -> Check:
        precondition = "live_credentials_isolated_withdrawal_disabled"
        citation = (
            '11_EXECUTION_GATES.md G11: "live credentials are provisioned only in isolated live '
            'infrastructure with withdrawal/transfer permissions disabled". 11_EXECUTION_GATES.md: '
            '"Waivers are prohibited for financial invariants, authorization, live credential '
            'isolation, reconciliation, halt controls, and recovery objectives." 06_SECURITY_AND_'
            'ACCESS_CONTROL.md section 6: "Live venue credentials are scoped to required API '
            'functions; withdrawal/transfer permissions are prohibited unless separately justified '
            'and approved, and are disabled by default." 01_SYSTEM_ARCHITECTURE.md section 4: "Live '
            'credentials are never available to lower environments."'
        )
        creds = _require_mapping(
            _get(config, "live_credentials", "proposed_live_config"),
            "proposed_live_config.live_credentials",
        )
        where = "proposed_live_config.live_credentials"
        if not _get_bool(creds, "isolated_infrastructure", where):
            return Check(
                precondition,
                False,
                "live credentials are not provisioned in isolated live infrastructure",
                citation,
            )
        if _get_bool(creds, "withdrawal_permission", where):
            return Check(
                precondition,
                False,
                "live credentials carry withdrawal permission; 06 section 6 prohibits it by default "
                "and G11 requires it disabled. This is a NON-WAIVABLE control.",
                citation,
            )
        if _get_bool(creds, "transfer_permission", where):
            return Check(
                precondition,
                False,
                "live credentials carry transfer permission; G11 requires withdrawal/transfer "
                "permissions disabled. This is a NON-WAIVABLE control.",
                citation,
            )
        if _get_bool(creds, "credentials_in_lower_environments", where):
            return Check(
                precondition,
                False,
                "live credentials are readable from a lower environment; 01 section 4: 'Live "
                "credentials are never available to lower environments.' This is a NON-WAIVABLE "
                "control.",
                citation,
            )
        environments = creds.get("provisioned_in_environments")
        if environments is not None:
            if isinstance(environments, str) or not isinstance(environments, Sequence):
                raise GuardInputError(
                    f"{where}.provisioned_in_environments: expected a list of environment names"
                )
            leaked = [e for e in environments if str(e) != "live"]
            if leaked:
                return Check(
                    precondition,
                    False,
                    f"live credentials are provisioned in non-live environment(s): {leaked}; "
                    "19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md section 2 requires live secrets to be "
                    "unreadable by lower environments",
                    citation,
                )
        return Check(
            precondition,
            True,
            "live credentials are isolated, withdrawal and transfer permissions are disabled, and no "
            "lower environment can read them",
            citation,
        )

    def _c11_owner_authorization_against_exact_digests(self, config: Mapping[str, Any]) -> Check:
        precondition = "owner_authorization_against_exact_digests"
        citation = (
            '11_EXECUTION_GATES.md G11: "owner authorization is recorded against the exact '
            'configuration/artifact digests". 25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_'
            'PROFILE.md section 8 item 9: "Gate report with binary PASS/FAIL, evidence references, '
            'reviewer identities, UTC timestamps, commit and artifact digests." 18_GOVERNANCE_DATA_'
            'AND_COMPLIANCE.md section 1: the owner is accountable for live activation.'
        )
        auth = _require_mapping(
            _get(config, "owner_authorization", "proposed_live_config"),
            "proposed_live_config.owner_authorization",
        )
        where = "proposed_live_config.owner_authorization"
        _get_str(auth, "owner_identity", where)
        config_digest = _get_str(auth, "config_digest", where)
        artifact_digest = _get_str(auth, "artifact_digest", where)
        if not _is_sha256_digest(config_digest):
            return Check(
                precondition,
                False,
                f"owner authorization records a malformed configuration digest {config_digest!r}; "
                "expected sha256:<64 hex>",
                citation,
            )
        if not _is_sha256_digest(artifact_digest):
            return Check(
                precondition,
                False,
                f"owner authorization records a malformed artifact digest {artifact_digest!r}; "
                "expected sha256:<64 hex>",
                citation,
            )
        # The recorded digests MUST equal the digests of the very artifacts being
        # activated. 11_EXECUTION_GATES.md G11 says "the exact" configuration and
        # artifact digests.
        live_config_digest = _get_str(config, "configuration_digest", "proposed_live_config")
        live_artifact_digest = _get_str(config, "artifact_digest", "proposed_live_config")
        if config_digest.lower() != live_config_digest.lower():
            return Check(
                precondition,
                False,
                f"owner authorization is recorded against configuration digest {config_digest} but the "
                f"proposed live configuration digest is {live_config_digest}; G11 requires authorization "
                "against the EXACT configuration digest",
                citation,
            )
        if artifact_digest.lower() != live_artifact_digest.lower():
            return Check(
                precondition,
                False,
                f"owner authorization is recorded against artifact digest {artifact_digest} but the "
                f"proposed live artifact digest is {live_artifact_digest}; G11 requires authorization "
                "against the EXACT artifact digest",
                citation,
            )
        if not _get_bool(auth, "evidence_immutable", where):
            return Check(
                precondition,
                False,
                "owner authorization evidence is not immutable; G11 requires the activation evidence to "
                "be immutable",
                citation,
            )
        return Check(
            precondition,
            True,
            f"owner authorization recorded by {_get_str(auth, 'owner_identity', where)} against the exact "
            "configuration and artifact digests, with immutable evidence",
            citation,
        )

    def _c12_canary_scope_and_abort_criteria(self, config: Mapping[str, Any]) -> Check:
        precondition = "canary_scope_and_abort_criteria"
        citation = (
            '11_EXECUTION_GATES.md G11: "a canary activation runs in the narrowest permitted scope; '
            'abort thresholds and the responsible operator are confirmed". 23_ARCHITECTURE_AND_'
            'COMPLIANCE_DECISIONS.md section 7: G11 "requires ... a narrow canary with explicit abort '
            'criteria". 25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_PROFILE.md section 8 item '
            '10: "Post-deployment verification plan, canary boundaries, abort criteria, rollback '
            'decision owner, and explicit completion record."'
        )
        canary = _require_mapping(
            _get(config, "canary", "proposed_live_config"), "proposed_live_config.canary"
        )
        where = "proposed_live_config.canary"
        _get_str(canary, "scope", where)
        _get_str_list(canary, "abort_thresholds", where)
        _get_str(canary, "responsible_operator", where)
        if not _get_bool(canary, "rollback_decision_owner_confirmed", where):
            return Check(
                precondition,
                False,
                "the rollback decision owner is not confirmed; 25 section 8 item 10 requires it",
                citation,
            )
        return Check(
            precondition,
            True,
            f"canary scope {canary['scope']!r} confirmed with abort thresholds and a named responsible "
            "operator and rollback decision owner",
            citation,
        )


# --------------------------------------------------------------------------
# Convenience entry points
# --------------------------------------------------------------------------


def load_json_document(path: str | Path) -> Any:
    """Load a JSON document, raising :class:`GuardInputError` on any problem.

    A missing, unreadable, or unparseable document is an error, which the
    evaluator converts into DENY. There is no "treat as empty" fallback, because
    18 section 5 requires the registry to fail closed.
    """
    p = Path(path)
    try:
        raw = p.read_text(encoding="utf-8")
    except FileNotFoundError as exc:
        raise GuardInputError(f"{p}: file does not exist") from exc
    except OSError as exc:
        raise GuardInputError(f"{p}: file could not be read ({exc})") from exc
    try:
        return json.loads(raw)
    except json.JSONDecodeError as exc:
        raise GuardInputError(f"{p}: file is not valid JSON ({exc})") from exc


def evaluate_from_files(
    config_path: str | Path,
    registry_path: str | Path,
    *,
    now: datetime | None = None,
) -> GuardResult:
    """Evaluate G11 from a proposed live configuration file and registry file.

    Any load or parse failure yields a DENY result. It never raises to the caller.
    """
    try:
        config = load_json_document(config_path)
        registry = load_json_document(registry_path)
    except GuardInputError as exc:
        return GuardResult(
            decision="DENY",
            checks=(
                Check(
                    precondition="input_integrity",
                    ok=False,
                    detail=f"evidence could not be loaded: {exc}",
                    citation=(
                        '11_EXECUTION_GATES.md: "Missing, stale, unverifiable, or contradictory '
                        'evidence is a release failure." 18_GOVERNANCE_DATA_AND_COMPLIANCE.md '
                        'section 5 requires fail-closed handling of unknown or unusable records.'
                    ),
                ),
            ),
        )
    guard = LiveActivationGuard(registry, now=now)
    return guard.evaluate(config)


def main(argv: Sequence[str] | None = None) -> int:
    """CLI entry point.

    Exit code 0 = ALLOW, 2 = DENY, 3 = input error (which is also a DENY).
    A DENY is never reported as a success.
    """
    args = list(sys.argv[1:] if argv is None else argv)
    if len(args) != 2:
        print(
            "usage: live_activation_guard <proposed_live_config.json> <eligibility_registry.json>",
            file=sys.stderr,
        )
        return 3
    result = evaluate_from_files(args[0], args[1])
    print(result.render())
    print(json.dumps(result.to_dict(), indent=2))
    return 0 if result.allowed else 2


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
