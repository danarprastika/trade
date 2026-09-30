package contracts_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aitc/trade/contracts"
)

// ---------------------------------------------------------------------------
// 03_CANONICAL_CONTRACTS.md: time.
// ---------------------------------------------------------------------------

func TestTimestampCanonicalFormIsUTCRFC3339Nanoseconds(t *testing.T) {
	t.Parallel()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		// Fall back to a fixed offset if the tzdata is unavailable.
		loc = time.FixedZone("X", -5*3600)
	}
	ts := contracts.NewTimestamp(time.Date(2026, 3, 15, 14, 30, 45, 123456789, loc))
	got := ts.String()
	// 2026-03-15 is inside US daylight saving time (UTC-4), so 14:30:45 local
	// normalises to 18:30:45Z.
	want := "2026-03-15T18:30:45.123456789Z"
	if got != want {
		t.Fatalf("String() = %q, want %q (non-UTC input must be normalised to UTC)", got, want)
	}
	rt, err := contracts.ParseTimestamp(got)
	if err != nil {
		t.Fatalf("ParseTimestamp: %v", err)
	}
	if !rt.Equal(ts) {
		t.Fatalf("round-trip lost precision: %v != %v", rt.UnixNano(), ts.UnixNano())
	}
}

func TestTimestampPreservesNanosecondPrecision(t *testing.T) {
	t.Parallel()
	for _, ns := range []int64{0, 1, 999, 1000, 123456789, 999999999} {
		ts := contracts.TimestampAt(ns)
		back, err := contracts.ParseTimestamp(ts.String())
		if err != nil {
			t.Fatalf("ParseTimestamp(%q): %v", ts.String(), err)
		}
		if back.UnixNano() != ns {
			t.Fatalf("precision lost for %d ns: got %d", ns, back.UnixNano())
		}
	}
}

func TestTimestampRejectsMalformedAndEmpty(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"", "   ", "not-a-time", "2026-13-01T00:00:00Z", "1752000000", "2026-03-15"} {
		if _, err := contracts.ParseTimestamp(s); err == nil {
			t.Fatalf("ParseTimestamp(%q) accepted a malformed timestamp", s)
		}
	}
	var zero contracts.Timestamp
	if _, err := zero.MarshalText(); err == nil {
		t.Fatal("marshalling a zero timestamp must fail; a missing timestamp is a defect")
	}
}

func TestTimestampAgeRejectsFutureTimestamps(t *testing.T) {
	t.Parallel()
	now := contracts.TimestampAt(1_752_000_000_000_000_000)
	fresh := now.Add(-2 * time.Second)
	age, err := fresh.Age(now)
	if err != nil {
		t.Fatalf("Age: %v", err)
	}
	if age != 2*time.Second {
		t.Fatalf("age = %s, want 2s", age)
	}
	// A future-dated source timestamp is a data-integrity defect that must be
	// surfaced, not clamped: otherwise a manipulated future timestamp could make
	// stale market data appear fresh to the freshness control.
	future := now.Add(time.Second)
	if _, err := future.Age(now); !errors.Is(err, contracts.ErrFutureTimestamp) {
		t.Fatalf("expected ErrFutureTimestamp for a future source timestamp, got %v", err)
	}
}

func TestEventTimesOrderingIsEnforced(t *testing.T) {
	t.Parallel()
	occ := contracts.TimestampAt(1000)
	rec := contracts.TimestampAt(2000)
	src := contracts.TimestampAt(500)
	ok := contracts.EventTimes{OccurredAt: occ, RecordedAt: rec, SourceTimestamp: &src}
	if err := ok.Validate(true); err != nil {
		t.Fatalf("valid event times rejected: %v", err)
	}
	// recorded_at preceding occurred_at indicates clock or pipeline corruption.
	bad := contracts.EventTimes{OccurredAt: rec, RecordedAt: occ}
	if err := bad.Validate(false); err == nil {
		t.Fatal("recorded_at preceding occurred_at must be rejected")
	}
	// Missing required source timestamp for a market-data event.
	noSrc := contracts.EventTimes{OccurredAt: occ, RecordedAt: rec}
	if err := noSrc.Validate(true); err == nil {
		t.Fatal("a market-data event without a source timestamp must be rejected")
	}
	if err := noSrc.Validate(false); err != nil {
		t.Fatalf("a purely internal event may omit source_timestamp: %v", err)
	}
	// Missing occurred_at / recorded_at.
	missing := contracts.EventTimes{RecordedAt: rec}
	if err := missing.Validate(false); err == nil {
		t.Fatal("missing occurred_at must be rejected")
	}
}

// ---------------------------------------------------------------------------
// 03_CANONICAL_CONTRACTS.md: error taxonomy.
// ---------------------------------------------------------------------------

func TestErrorTaxonomyIsClosedAndMapped(t *testing.T) {
	t.Parallel()
	want := map[contracts.ErrorCode]int{
		contracts.CodeValidation:            http.StatusBadRequest,          // 400
		contracts.CodeAuthentication:        http.StatusUnauthorized,        // 401
		contracts.CodeAuthorization:         http.StatusForbidden,           // 403
		contracts.CodeConflict:              http.StatusConflict,            // 409
		contracts.CodeRiskRejected:          http.StatusUnprocessableEntity, // 422
		contracts.CodeDataStale:             http.StatusUnprocessableEntity, // 422
		contracts.CodeReconciliationReq:     http.StatusUnprocessableEntity, // 422
		contracts.CodeRateLimited:           http.StatusTooManyRequests,     // 429
		contracts.CodeDependencyUnavailable: http.StatusServiceUnavailable,  // 503
		contracts.CodeTimeoutUnknown:        http.StatusServiceUnavailable,  // 503
		contracts.CodeInternal:              http.StatusInternalServerError, // 500
	}
	got := contracts.CanonicalErrorCodes()
	if len(got) != len(want) {
		t.Fatalf("taxonomy has %d members, want exactly %d: %v", len(got), len(want), got)
	}
	for code, status := range want {
		if !code.Known() {
			t.Fatalf("code %q is missing from the taxonomy", code)
		}
		if code.HTTPStatus() != status {
			t.Fatalf("code %q maps to %d, want %d", code, code.HTTPStatus(), status)
		}
	}
}

func TestUnknownErrorCodeIsNotEmitted(t *testing.T) {
	t.Parallel()
	// An unknown code is a contract violation; it must be coerced to
	// INTERNAL_ERROR and recorded as a defect rather than emitted verbatim.
	unknown := contracts.ErrorCode("TEAPOT")
	if unknown.Known() {
		t.Fatal("unknown code reported as known")
	}
	e := contracts.Errorf(unknown, "something failed")
	if e.Code != contracts.CodeInternal {
		t.Fatalf("non-taxonomy code escaped as %q", e.Code)
	}
	if !strings.Contains(e.Message, "non-taxonomy") {
		t.Fatalf("defect was not recorded in the message: %q", e.Message)
	}
}

func TestTimeoutUnknownIsNotRetryable(t *testing.T) {
	t.Parallel()
	// Retrying a financial command whose outcome is unknown risks duplicate
	// exposure (01_SYSTEM_ARCHITECTURE.md invariant 3). Only rate limiting and
	// an unavailable dependency are retryable.
	if contracts.CodeTimeoutUnknown.Retryable() {
		t.Fatal("TIMEOUT_UNKNOWN must not advertise itself as retryable")
	}
	if !contracts.CodeRateLimited.Retryable() || !contracts.CodeDependencyUnavailable.Retryable() {
		t.Fatal("RATE_LIMITED and DEPENDENCY_UNAVAILABLE should be retryable")
	}
	for _, c := range []contracts.ErrorCode{
		contracts.CodeRiskRejected, contracts.CodeAuthorization,
		contracts.CodeValidation, contracts.CodeConflict,
		contracts.CodeDataStale, contracts.CodeReconciliationReq,
	} {
		if c.Retryable() {
			t.Fatalf("%s must not be retryable", c)
		}
	}
}

func TestErrorWireDropsInternalCause(t *testing.T) {
	t.Parallel()
	// InternalDetailf is the constructor for any failure whose diagnostic could
	// contain hostnames, addresses, SQL or vendor error text. The client sees a
	// fixed opaque message; the detail is retained only as the internal cause.
	secret := contracts.InternalDetailf(
		"an internal error occurred",
		"insert into ledger.entry on 10.0.3.17:5432 failed: %w",
		errors.New("pq: connection refused"),
	).WithDetail("component", "ledger").WithCorrelation("cor-abc123")

	w := secret.Wire()
	if w.Message != "an internal error occurred" {
		t.Fatalf("client message = %q, want the fixed opaque message", w.Message)
	}
	for _, leak := range []string{"10.0.3.17", "ledger.entry", "connection refused", "pq:"} {
		if strings.Contains(w.Message, leak) {
			t.Fatalf("internal detail %q leaked into the client body: %q", leak, w.Message)
		}
	}
	if w.CorrelationID != "cor-abc123" {
		t.Fatalf("correlation id lost: %q", w.CorrelationID)
	}
	if w.Details["component"] != "ledger" {
		t.Fatalf("safe detail lost: %v", w.Details)
	}
	// The internal cause must remain available to operators via errors.Is/As.
	if !errors.Is(secret, contracts.ErrInternal) {
		t.Fatal("errors.Is must match the taxonomy class")
	}
	if !strings.Contains(secret.Error(), "10.0.3.17") {
		t.Fatal("the internal diagnostic must remain available to operators")
	}
}

func TestErrorWithDetailDoesNotMutateReceiver(t *testing.T) {
	t.Parallel()
	base := contracts.ErrValidationf("bad input")
	a := base.WithDetail("field", "quantity")
	b := base.WithDetail("field", "price")
	if len(base.Details) != 0 {
		t.Fatal("WithDetail mutated the receiver; canonical errors must be immutable")
	}
	if a.Details["field"] != "quantity" || b.Details["field"] != "price" {
		t.Fatalf("derived errors aliased a shared map: %v %v", a.Details, b.Details)
	}
}

// ---------------------------------------------------------------------------
// 03_CANONICAL_CONTRACTS.md: command and event envelopes.
// ---------------------------------------------------------------------------

func validCommand() contracts.CommandEnvelope {
	return contracts.CommandEnvelope{
		CommandID:      contracts.MustID(contracts.EntityCommand),
		CommandType:    "oms.submit_order",
		SchemaVersion:  contracts.CurrentSchemaVersion,
		CorrelationID:  "cor-0001",
		CausationID:    "cmd-parent",
		ActorID:        "sub-0001",
		ActorType:      contracts.ActorHuman,
		Environment:    contracts.EnvPaper,
		RequestedAt:    contracts.TimestampAt(1_752_000_000_000_000_000),
		IdempotencyKey: "idem-key-0001",
		Payload:        []byte(`{"instrument_id":"ins_1"}`),
	}
}

func TestCommandEnvelopeValidatesRequiredFields(t *testing.T) {
	t.Parallel()
	base := validCommand()
	if err := base.Validate(); err != nil {
		t.Fatalf("valid command rejected: %v", err)
	}

	mutations := map[string]func(*contracts.CommandEnvelope){
		"missing command_id":      func(c *contracts.CommandEnvelope) { c.CommandID = contracts.ID{} },
		"wrong command_id prefix": func(c *contracts.CommandEnvelope) { c.CommandID = contracts.MustID(contracts.EntityOrder) },
		"missing command_type":    func(c *contracts.CommandEnvelope) { c.CommandType = "" },
		"unsupported schema":      func(c *contracts.CommandEnvelope) { c.SchemaVersion = "99.0.0" },
		"missing correlation_id":  func(c *contracts.CommandEnvelope) { c.CorrelationID = "" },
		"missing actor_id":        func(c *contracts.CommandEnvelope) { c.ActorID = "" },
		"unknown actor_type":      func(c *contracts.CommandEnvelope) { c.ActorType = "ROBOT" },
		"unknown environment":     func(c *contracts.CommandEnvelope) { c.Environment = "prod" },
		"missing requested_at":    func(c *contracts.CommandEnvelope) { c.RequestedAt = contracts.Timestamp{} },
		"missing idempotency_key": func(c *contracts.CommandEnvelope) { c.IdempotencyKey = "" },
		"invalid json payload":    func(c *contracts.CommandEnvelope) { c.Payload = []byte(`{`) },
		"payload over size limit": func(c *contracts.CommandEnvelope) {
			c.Payload = []byte(`"` + strings.Repeat("x", contracts.MaxCommandPayloadBytes) + `"`)
		},
		"correlation id injection": func(c *contracts.CommandEnvelope) { c.CorrelationID = "abc\nrm -rf /" },
		"oversized correlation id": func(c *contracts.CommandEnvelope) { c.CorrelationID = strings.Repeat("a", 200) },
	}
	for name, mutate := range mutations {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := base
			mutate(&c)
			if err := c.Validate(); err == nil {
				t.Fatalf("command with %s was accepted", name)
			}
		})
	}
}

func TestCommandPayloadLimitIsEnforcedAtTheBoundary(t *testing.T) {
	t.Parallel()
	// 23_ARCHITECTURE_AND_COMPLIANCE_DECISIONS.md §7: 64 KiB command limit.
	if contracts.MaxCommandPayloadBytes != 64*1024 {
		t.Fatalf("command payload limit is %d, want 65536", contracts.MaxCommandPayloadBytes)
	}
	if contracts.MaxRequestBodyBytes != 1024*1024 {
		t.Fatalf("request body limit is %d, want 1048576", contracts.MaxRequestBodyBytes)
	}
	atLimit := make([]byte, contracts.MaxCommandPayloadBytes)
	for i := range atLimit {
		atLimit[i] = 'a'
	}
	atLimit[0] = '"'
	atLimit[len(atLimit)-1] = '"'
	c := validCommand()
	c.Payload = atLimit
	if err := c.Validate(); err != nil {
		t.Fatalf("payload exactly at the limit was rejected: %v", err)
	}
	c.Payload = append(atLimit, ' ')
	if err := c.Validate(); err == nil {
		t.Fatal("payload over the limit was accepted")
	}
}

func validEvent() contracts.EventEnvelope {
	return contracts.EventEnvelope{
		EventID:       contracts.MustID(contracts.EntityEvent),
		EventType:     "order.acknowledged",
		SchemaVersion: contracts.CurrentSchemaVersion,
		AggregateType: "order",
		AggregateID:   "ord_1",
		CorrelationID: "cor-0001",
		CausationID:   "cmd_1",
		ProducerID:    "control-plane",
		OccurredAt:    contracts.TimestampAt(1_752_000_000_000_000_000),
		RecordedAt:    contracts.TimestampAt(1_752_000_000_000_000_001),
		Sequence:      1,
		Payload:       []byte(`{"state":"ACKNOWLEDGED"}`),
	}
}

func TestEventEnvelopeValidatesRequiredFields(t *testing.T) {
	t.Parallel()
	base := validEvent()
	if err := base.Validate(false); err != nil {
		t.Fatalf("valid event rejected: %v", err)
	}
	mutations := map[string]func(*contracts.EventEnvelope){
		"missing event_id":       func(e *contracts.EventEnvelope) { e.EventID = contracts.ID{} },
		"wrong event_id prefix":  func(e *contracts.EventEnvelope) { e.EventID = contracts.MustID(contracts.EntityCommand) },
		"missing event_type":     func(e *contracts.EventEnvelope) { e.EventType = "" },
		"unsupported schema":     func(e *contracts.EventEnvelope) { e.SchemaVersion = "2.0.0" },
		"missing aggregate_type": func(e *contracts.EventEnvelope) { e.AggregateType = "" },
		"missing aggregate_id":   func(e *contracts.EventEnvelope) { e.AggregateID = "" },
		"missing producer_id":    func(e *contracts.EventEnvelope) { e.ProducerID = "" },
		"missing occurred_at":    func(e *contracts.EventEnvelope) { e.OccurredAt = contracts.Timestamp{} },
		"recorded before occurred": func(e *contracts.EventEnvelope) {
			e.OccurredAt = contracts.TimestampAt(2000)
			e.RecordedAt = contracts.TimestampAt(1000)
		},
		"zero sequence":     func(e *contracts.EventEnvelope) { e.Sequence = 0 },
		"negative sequence": func(e *contracts.EventEnvelope) { e.Sequence = -1 },
		"invalid payload":   func(e *contracts.EventEnvelope) { e.Payload = []byte("not json") },
	}
	for name, mutate := range mutations {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := base
			mutate(&e)
			if err := e.Validate(false); err == nil {
				t.Fatalf("event with %s was accepted", name)
			}
		})
	}
}

func TestUnsupportedSchemaVersionIsNotSilentlyMapped(t *testing.T) {
	t.Parallel()
	// 03_CANONICAL_CONTRACTS.md, "Compatibility": unknown values are treated as
	// unsupported, not silently mapped.
	if contracts.SchemaVersion("1.1.0").SupportedSchemaVersion() {
		t.Fatal("a future minor version must be unsupported until a coordinated migration lands")
	}
	if err := contracts.SchemaVersion("1.1.0").Validate(); err == nil {
		t.Fatal("unsupported schema version was accepted")
	}
	if !contracts.CurrentSchemaVersion.SupportedSchemaVersion() {
		t.Fatal("the emitted schema version must be in the supported set")
	}
}

func TestTraceContextDerivation(t *testing.T) {
	t.Parallel()
	tc, err := contracts.NewTrace("cor-1", "cmd-1")
	if err != nil {
		t.Fatalf("NewTrace: %v", err)
	}
	if tc.CorrelationID != "cor-1" || tc.CausationID != "cmd-1" {
		t.Fatalf("trace = %+v", tc)
	}
	if _, err := contracts.NewTrace("", "cmd-1"); err == nil {
		t.Fatal("empty correlation id must be rejected")
	}
	if _, err := contracts.NewTrace("cor-1", ""); err == nil {
		t.Fatal("empty causation id must be rejected")
	}
}

// ---------------------------------------------------------------------------
// 03_CANONICAL_CONTRACTS.md: idempotency.
// ---------------------------------------------------------------------------

func TestIdempotencyScopeValidation(t *testing.T) {
	t.Parallel()
	good := contracts.IdempotencyScope{
		ActorID:     "sub-1",
		Endpoint:    "POST /api/v1/orders",
		Environment: contracts.EnvPaper,
		Key:         "client-key-0001",
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid scope rejected: %v", err)
	}
	bad := []contracts.IdempotencyScope{
		{ActorID: "", Endpoint: "E", Environment: contracts.EnvPaper, Key: "client-key-0001"},
		{ActorID: "a", Endpoint: "", Environment: contracts.EnvPaper, Key: "client-key-0001"},
		{ActorID: "a", Endpoint: "E", Environment: "prod", Key: "client-key-0001"},
		{ActorID: "a", Endpoint: "E", Environment: contracts.EnvPaper, Key: "short"},
		{ActorID: "a", Endpoint: "E", Environment: contracts.EnvPaper, Key: ""},
		{ActorID: "a", Endpoint: "E", Environment: contracts.EnvPaper, Key: "key with space1"},
		{ActorID: "a", Endpoint: "E", Environment: contracts.EnvPaper, Key: "key\nnewline1"},
		{ActorID: "a", Endpoint: "E", Environment: contracts.EnvPaper, Key: strings.Repeat("k", 129)},
	}
	for i, s := range bad {
		if err := s.Validate(); err == nil {
			t.Fatalf("invalid idempotency scope %d accepted: %+v", i, s)
		}
	}
}

func TestIdempotencyScopeHashIsUnambiguous(t *testing.T) {
	t.Parallel()
	// Length-prefixed field encoding must prevent a separator shift from making
	// two different scopes collide.
	a := contracts.IdempotencyScope{ActorID: "ab", Endpoint: "c", Environment: contracts.EnvDev, Key: "key-00001"}
	b := contracts.IdempotencyScope{ActorID: "a", Endpoint: "bc", Environment: contracts.EnvDev, Key: "key-00001"}
	if a.Hash() == b.Hash() {
		t.Fatal("distinct idempotency scopes collided in the scope hash")
	}
	// Identical scopes must hash identically (durable replay lookup depends on it).
	if a.Hash() != a.Hash() {
		t.Fatal("scope hash is not deterministic")
	}
}

func TestDecideReplayEnforcesNoDuplicateExposure(t *testing.T) {
	t.Parallel()
	scope := contracts.IdempotencyScope{
		ActorID:     "sub-1",
		Endpoint:    "POST /api/v1/orders",
		Environment: contracts.EnvPaper,
		Key:         "client-key-0001",
	}
	digest := contracts.DigestRequest([]byte(`{"instrument_id":"ins_1"}`))

	// No prior record: execute.
	dec, err := contracts.DecideReplay(nil, scope, digest)
	if err != nil || dec != contracts.ReplayExecute {
		t.Fatalf("first submission = %q, %v; want execute", dec, err)
	}

	// In-flight record: reject rather than execute concurrently.
	inflight := &contracts.IdempotencyRecord{
		ScopeHash: scope.Hash(), State: contracts.IdempotencyInFlight, RequestDigest: digest,
	}
	dec, err = contracts.DecideReplay(inflight, scope, digest)
	if err != nil || dec != contracts.ReplayInFlight {
		t.Fatalf("concurrent replay = %q, %v; want in_flight", dec, err)
	}

	// Terminal results replay verbatim.
	for _, state := range []contracts.IdempotencyState{
		contracts.IdempotencySucceeded, contracts.IdempotencyFailed, contracts.IdempotencyUnknown,
	} {
		rec := &contracts.IdempotencyRecord{
			ScopeHash: scope.Hash(), State: state, RequestDigest: digest,
		}
		dec, err = contracts.DecideReplay(rec, scope, digest)
		if err != nil || dec != contracts.ReplayReturnStored {
			t.Fatalf("replay of %q = %q, %v; want return_stored", state, dec, err)
		}
		if !state.Terminal() {
			t.Fatalf("state %q should be terminal", state)
		}
	}

	// Same key, different body: CONFLICT. Executing would void the guarantee.
	mismatch := &contracts.IdempotencyRecord{
		ScopeHash: scope.Hash(), State: contracts.IdempotencySucceeded, RequestDigest: "different",
	}
	dec, err = contracts.DecideReplay(mismatch, scope, digest)
	if err != nil || dec != contracts.ReplayKeyReuseMismatch {
		t.Fatalf("key reuse with a different body = %q, %v; want key_reuse_mismatch", dec, err)
	}

	// A lookup that returned a record for a different scope fails closed.
	wrongScope := &contracts.IdempotencyRecord{
		ScopeHash: contracts.IdempotencyScope{
			ActorID: "other", Endpoint: scope.Endpoint,
			Environment: scope.Environment, Key: scope.Key,
		}.Hash(),
		State: contracts.IdempotencySucceeded, RequestDigest: digest,
	}
	if _, err := contracts.DecideReplay(wrongScope, scope, digest); err == nil {
		t.Fatal("a scope mismatch must fail closed rather than execute")
	}
}

func TestIdempotencyInFlightIsNotTerminal(t *testing.T) {
	t.Parallel()
	if contracts.IdempotencyInFlight.Terminal() {
		t.Fatal("in_flight must not be terminal")
	}
}

// ---------------------------------------------------------------------------
// Scope, environment and identity semantics.
// ---------------------------------------------------------------------------

func TestEnvironmentPromotionIsMonotonic(t *testing.T) {
	t.Parallel()
	ladder := []contracts.Environment{
		contracts.EnvDev, contracts.EnvTest, contracts.EnvStaging,
		contracts.EnvPaper, contracts.EnvShadow, contracts.EnvLive,
	}
	for i := range ladder {
		if !ladder[i].Known() {
			t.Fatalf("%s is not in the closed environment set", ladder[i])
		}
		for j := range ladder {
			got := ladder[i].IsAtLeast(ladder[j])
			want := i >= j
			if got != want {
				t.Fatalf("%s.IsAtLeast(%s) = %v, want %v", ladder[i], ladder[j], got, want)
			}
		}
	}
	if contracts.Environment("prod").Known() {
		t.Fatal("an unknown environment must not be accepted")
	}
	// Forward promotion is permitted; backward promotion is a rollback that
	// requires an explicit audited record, not a promotion.
	if err := contracts.EnvironmentForPromotion(contracts.EnvPaper, contracts.EnvLive); err != nil {
		t.Fatalf("forward promotion rejected: %v", err)
	}
	if err := contracts.EnvironmentForPromotion(contracts.EnvLive, contracts.EnvPaper); err == nil {
		t.Fatal("backward promotion must be rejected")
	}
}

func TestUnknownTradingStatusIsNotTradable(t *testing.T) {
	t.Parallel()
	// A status that cannot be established fails closed.
	for _, s := range []contracts.TradingStatus{
		contracts.StatusUnknown, contracts.StatusHalted, contracts.StatusClosed,
		contracts.StatusSessionBreak, contracts.StatusDelisted,
		contracts.StatusPreOpen, contracts.StatusPostClose,
	} {
		if s.Tradable() {
			t.Fatalf("%s must not be tradable", s)
		}
	}
	if !contracts.StatusOpen.Tradable() {
		t.Fatal("OPEN must be tradable")
	}
	if contracts.TradingStatus("WEIRD").Known() {
		t.Fatal("an unknown trading status must not be reported as known")
	}
}

func TestOrderTypeRiskClassification(t *testing.T) {
	t.Parallel()
	// Risk-reducing types bypass the full risk-increasing control set and are
	// governed by the separate safe-reduction policy instead.
	for _, o := range []contracts.OrderType{contracts.OrderReduceOnly, contracts.OrderClosePosition} {
		if o.RiskIncreasing() {
			t.Fatalf("%s must be classified as risk-reducing", o)
		}
		if !o.Valid() {
			t.Fatalf("%s must be a valid order type", o)
		}
	}
	for _, o := range []contracts.OrderType{
		contracts.OrderMarket, contracts.OrderLimit, contracts.OrderStop,
		contracts.OrderStopLimit, contracts.OrderPostOnly, contracts.OrderIOC, contracts.OrderFOK,
	} {
		if !o.RiskIncreasing() {
			t.Fatalf("%s must be classified as risk-increasing", o)
		}
	}
	if contracts.OrderType("ICEBERG").Valid() {
		t.Fatal("an unknown order type must be unsupported, not silently accepted")
	}
}

func TestActorValidationFailsClosed(t *testing.T) {
	t.Parallel()
	valid := contracts.Actor{
		ID:           "sub-1",
		Type:         contracts.ActorHuman,
		SessionID:    "ses-1",
		AuthStrength: contracts.AuthPhishingResistant,
		AuthTime:     contracts.TimestampAt(1_752_000_000_000_000_000),
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid human actor rejected: %v", err)
	}
	// A human without a server-side session cannot have step-up evaluated.
	noSession := valid
	noSession.SessionID = ""
	if err := noSession.Validate(); err == nil {
		t.Fatal("a human actor without a session must be rejected")
	}
	noAuthTime := valid
	noAuthTime.AuthTime = contracts.Timestamp{}
	if err := noAuthTime.Validate(); err == nil {
		t.Fatal("a human actor without auth_time must be rejected")
	}
	unknownType := valid
	unknownType.Type = "SUPERUSER"
	if err := unknownType.Validate(); err == nil {
		t.Fatal("an unknown actor type must be rejected")
	}
	// A workload needs an audience and workload-identity assurance.
	wl := contracts.Actor{ID: "svc-1", Type: contracts.ActorWorkload, AuthStrength: contracts.AuthPhishingResistant}
	if err := wl.Validate(); err == nil {
		t.Fatal("a workload without an audience must be rejected")
	}
	wl.WorkloadAudience = "control-plane"
	if err := wl.Validate(); err != nil {
		t.Fatalf("valid workload actor rejected: %v", err)
	}
	weak := wl
	weak.AuthStrength = contracts.AuthPassword
	if err := weak.Validate(); err == nil {
		t.Fatal("a workload with password-only assurance must be rejected")
	}
}

func TestAuthStrengthOrdering(t *testing.T) {
	t.Parallel()
	if !contracts.AuthPhishingResistant.AtLeast(contracts.AuthMFA) {
		t.Fatal("phishing-resistant must satisfy an MFA requirement")
	}
	if !contracts.AuthMFA.AtLeast(contracts.AuthPassword) {
		t.Fatal("MFA must satisfy a password requirement")
	}
	if contracts.AuthPassword.AtLeast(contracts.AuthPhishingResistant) {
		t.Fatal("password must not satisfy a phishing-resistant requirement")
	}
	if contracts.AuthNone.AtLeast(contracts.AuthPassword) {
		t.Fatal("NONE must not satisfy any requirement")
	}
	if contracts.AuthStrength("MAGIC").AtLeast(contracts.AuthNone) {
		t.Fatal("an unknown strength must not satisfy a requirement")
	}
}

func TestActorRedactionDropsSessionAndDisplay(t *testing.T) {
	t.Parallel()
	a := contracts.Actor{
		ID: "sub-1", Type: contracts.ActorHuman, Display: "Dana Redacted",
		SessionID: "ses-secret", AuthStrength: contracts.AuthMFA,
	}
	r := a.Redacted()
	if r.SessionID != "" || r.Display != "" {
		t.Fatalf("redaction left sensitive fields: %+v", r)
	}
	if r.ID != a.ID || r.Type != a.Type {
		t.Fatalf("redaction destroyed the correlating fields: %+v", r)
	}
	if a.SessionID != "ses-secret" {
		t.Fatal("redaction mutated the original actor")
	}
}

func TestSideAndMarketClassVocabulary(t *testing.T) {
	t.Parallel()
	if contracts.SideBuy.Sign() != 1 || contracts.SideSell.Sign() != -1 {
		t.Fatal("side sign convention is wrong")
	}
	if contracts.SideBuy.Opposite() != contracts.SideSell {
		t.Fatal("Opposite is wrong for BUY")
	}
	if contracts.Side("SHORT").Valid() {
		t.Fatal("an unknown side must be unsupported")
	}
	classes := contracts.MarketClasses()
	if len(classes) != 5 {
		t.Fatalf("market class set has %d members, want 5: %v", len(classes), classes)
	}
	for _, m := range classes {
		if !m.Known() {
			t.Fatalf("%s is not a known market class", m)
		}
	}
	if contracts.MarketClass("banking").Known() {
		t.Fatal("an unknown market class must be unsupported")
	}
}

func TestInstrumentValidation(t *testing.T) {
	t.Parallel()
	i := contracts.Instrument{
		ID:             contracts.MustID(contracts.EntityInstrument),
		MarketClass:    contracts.MarketCrypto,
		Base:           "BTC",
		Quote:          "USDT",
		TradingStatus:  contracts.StatusOpen,
		PriceIncrement: contracts.MustParseDecimal("0.10"),
	}
	if err := i.Validate(); err != nil {
		t.Fatalf("valid instrument rejected: %v", err)
	}
	bad := i
	bad.MarketClass = "forex"
	if err := bad.Validate(); err == nil {
		t.Fatal("an unknown market class must be rejected")
	}
	bad = i
	bad.TradingStatus = "SOMEDAY"
	if err := bad.Validate(); err == nil {
		t.Fatal("an unknown trading status must be rejected")
	}
	bad = i
	bad.Base = "b"
	if err := bad.Validate(); err == nil {
		t.Fatal("an invalid base currency must be rejected")
	}
	bad = i
	bad.PriceIncrement = contracts.MustParseDecimal("0")
	if err := bad.Validate(); err == nil {
		t.Fatal("a zero price increment must be rejected; unknown precision is a deny condition")
	}
}
