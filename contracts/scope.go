package contracts

import (
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// Environment, market and identity scope.
//
// 01_SYSTEM_ARCHITECTURE.md §4: dev/test/staging/paper/shadow/live each have
// separate credentials, database, keys, deployment identity, network policy and
// configuration namespace. Environment is therefore part of every command,
// event, audit record and authorization decision.
// ---------------------------------------------------------------------------

// Environment is the deployment scope of a request or record. Closed set.
type Environment string

const (
	EnvDev     Environment = "dev"
	EnvTest    Environment = "test"
	EnvStaging Environment = "staging"
	EnvPaper   Environment = "paper"
	EnvShadow  Environment = "shadow"
	EnvLive    Environment = "live"
)

// promotionOrder is the environment promotion ladder
// (09_TESTING_AND_RELEASE_EVIDENCE.md, "Release strategy"). Promotion is
// monotonic: a configuration or artifact may move forward but never backward
// without an explicit, audited rollback record.
var promotionOrder = []Environment{EnvDev, EnvTest, EnvStaging, EnvPaper, EnvShadow, EnvLive}

var environmentSet = func() map[Environment]struct{} {
	m := make(map[Environment]struct{}, len(promotionOrder))
	for _, e := range promotionOrder {
		m[e] = struct{}{}
	}
	return m
}()

// Known reports whether the environment is a member of the closed set. Unknown
// values are unsupported and must fail closed.
func (e Environment) Known() bool { _, ok := environmentSet[e]; return ok }

// Rank returns the promotion position, or -1 when unknown.
func (e Environment) Rank() int {
	for i, v := range promotionOrder {
		if v == e {
			return i
		}
	}
	return -1
}

// IsAtLeast reports whether e is at or after other on the promotion ladder.
func (e Environment) IsAtLeast(other Environment) bool {
	a, b := e.Rank(), other.Rank()
	return a >= 0 && b >= 0 && a >= b
}

// IsLive reports the live environment. Live is the only environment in which
// real-money submission is even conceivable, and it is disabled by default.
func (e Environment) IsLive() bool { return e == EnvLive }

// Validate rejects an unknown environment.
func (e Environment) Validate() error {
	if !e.Known() {
		return fmt.Errorf("%w: unknown environment %q", ErrInvalidIdentifier, string(e))
	}
	return nil
}

// EnvironmentForPromotion resolves a promotion target and enforces monotonicity.
func EnvironmentForPromotion(from, to Environment) error {
	if err := from.Validate(); err != nil {
		return err
	}
	if err := to.Validate(); err != nil {
		return err
	}
	if from.IsAtLeast(to) {
		return fmt.Errorf("%w: promotion from %s to %s is not forward-moving",
			ErrInvalidIdentifier, from, to)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Market scope
// ---------------------------------------------------------------------------

// MarketClass is the independently enabled market scope
// (16_MARKET_DATA_AND_VENUE_ADAPTERS.md §5). Each class is enabled separately;
// enabling one never implies another.
type MarketClass string

const (
	MarketCrypto     MarketClass = "crypto"
	MarketFX         MarketClass = "fx"
	MarketEquities   MarketClass = "equities"
	MarketCommodity  MarketClass = "commodities"
	MarketDerivative MarketClass = "derivatives"
)

var marketClasses = []MarketClass{MarketCrypto, MarketFX, MarketEquities, MarketCommodity, MarketDerivative}

var marketSet = func() map[MarketClass]struct{} {
	m := make(map[MarketClass]struct{}, len(marketClasses))
	for _, c := range marketClasses {
		m[c] = struct{}{}
	}
	return m
}()

// Known reports membership of the closed market-class set.
func (m MarketClass) Known() bool { _, ok := marketSet[m]; return ok }

// Validate rejects an unknown market class.
func (m MarketClass) Validate() error {
	if !m.Known() {
		return fmt.Errorf("%w: unknown market class %q", ErrInvalidIdentifier, string(m))
	}
	return nil
}

// MarketClasses returns the closed set in stable order.
func MarketClasses() []MarketClass {
	out := append([]MarketClass(nil), marketClasses...)
	return out
}

// TradingStatus is the platform's view of whether an instrument may be traded.
// Venue status is authoritative where available and is never inferred from a
// wall-clock schedule alone (16_..._ADAPTERS.md §3).
type TradingStatus string

const (
	StatusOpen         TradingStatus = "OPEN"
	StatusClosed       TradingStatus = "CLOSED"
	StatusHalted       TradingStatus = "HALTED"
	StatusAuction      TradingStatus = "AUCTION"
	StatusSessionBreak TradingStatus = "SESSION_BREAK"
	StatusPreOpen      TradingStatus = "PRE_OPEN"
	StatusPostClose    TradingStatus = "POST_CLOSE"
	StatusDelisted     TradingStatus = "DELISTED"
	// StatusUnknown is the fail-closed status for an instrument whose tradability
	// could not be established. It is never tradable.
	StatusUnknown TradingStatus = "UNKNOWN"
)

// statusSet is the closed trading-status set.
var statusSet = map[TradingStatus]struct{}{
	StatusOpen: {}, StatusClosed: {}, StatusHalted: {}, StatusAuction: {},
	StatusSessionBreak: {}, StatusPreOpen: {}, StatusPostClose: {},
	StatusDelisted: {}, StatusUnknown: {},
}

// Known reports membership of the closed trading-status set.
func (s TradingStatus) Known() bool { _, ok := statusSet[s]; return ok }

// Tradable reports whether the status permits a new risk-increasing order.
// UNKNOWN is not tradable: an unverifiable status fails closed.
func (s TradingStatus) Tradable() bool { return s == StatusOpen }

// ---------------------------------------------------------------------------
// Actor identity
// ---------------------------------------------------------------------------

// ActorType distinguishes human, workload and system actors
// (03_CANONICAL_CONTRACTS.md, "Command envelope"). A workload can never
// impersonate a human, and a human can never act as a workload.
type ActorType string

const (
	ActorHuman      ActorType = "HUMAN"
	ActorWorkload   ActorType = "WORKLOAD"
	ActorSystem     ActorType = "SYSTEM"
	ActorAgent      ActorType = "AGENT"
	ActorBreakGlass ActorType = "BREAK_GLASS"
)

var actorSet = map[ActorType]struct{}{
	ActorHuman: {}, ActorWorkload: {}, ActorSystem: {}, ActorAgent: {}, ActorBreakGlass: {},
}

// Known reports membership of the closed actor-type set.
func (a ActorType) Known() bool { _, ok := actorSet[a]; return ok }

// IsNonHuman reports whether the actor is machine, which is subject to workload
// identity, audience-bound credentials and delegation attenuation.
func (a ActorType) IsNonHuman() bool {
	return a == ActorWorkload || a == ActorSystem || a == ActorAgent
}

// Actor identifies the principal performing a command. It is carried in every
// command envelope, event envelope and audit record.
type Actor struct {
	// ID is the stable platform subject identifier, or the workload principal
	// for a non-human actor.
	ID string
	// Type is the actor class.
	Type ActorType
	// Display is an optional operator-facing label. It is not authoritative and
	// must not be used for authorization decisions.
	Display string
	// SessionID binds the actor to a server-side session for human actors.
	SessionID string
	// AuthStrength is the phishing-resistant assurance level established for
	// this session (21_ZERO_TRUST_SSO_AND_AUTHORIZATION.md §2).
	AuthStrength AuthStrength
	// AuthTime is when the current authentication (or step-up) occurred. It is
	// the value compared against step-up freshness requirements.
	AuthTime Timestamp
	// WorkloadAudience is the intended audience for a workload token; a token
	// presented to a different audience is rejected.
	WorkloadAudience string
	// DelegatedBy records the initiating human when a workload acts on behalf
	// of a person. Delegation is attenuated and expires with the task.
	DelegatedBy string
}

// AuthStrength is the authentication assurance level of a session.
type AuthStrength string

const (
	// AuthNone means no verified authentication. Every privileged action fails.
	AuthNone AuthStrength = "NONE"
	// AuthPassword is single-factor; insufficient for any privileged action.
	AuthPassword AuthStrength = "PASSWORD"
	// AuthMFA is multi-factor but not phishing-resistant.
	AuthMFA AuthStrength = "MFA"
	// AuthPhishingResistant is FIDO2/WebAuthn or an equivalent hardware factor.
	// Required for privileged roles and for every step-up-gated action.
	AuthPhishingResistant AuthStrength = "PHISHING_RESISTANT"
)

var authStrengthRank = map[AuthStrength]int{
	AuthNone: 0, AuthPassword: 1, AuthMFA: 2, AuthPhishingResistant: 3,
}

// Rank returns the assurance rank for comparison.
func (a AuthStrength) Rank() int {
	if r, ok := authStrengthRank[a]; ok {
		return r
	}
	return -1
}

// Known reports membership of the closed auth-strength set.
func (a AuthStrength) Known() bool { _, ok := authStrengthRank[a]; return ok }

// AtLeast reports whether the strength satisfies a required level.
func (a AuthStrength) AtLeast(required AuthStrength) bool {
	return a.Rank() >= required.Rank() && a.Rank() >= 0
}

// Actor validation is deliberately strict: an actor that cannot be fully
// identified and validated must be rejected so that the affected sensitive
// operation is denied (execution directive §9).
func (a Actor) Validate() error {
	if strings.TrimSpace(a.ID) == "" {
		return fmt.Errorf("%w: actor_id is required", ErrInvalidIdentifier)
	}
	if !a.Type.Known() {
		return fmt.Errorf("%w: unknown actor_type %q", ErrInvalidIdentifier, string(a.Type))
	}
	if !a.AuthStrength.Known() {
		return fmt.Errorf("%w: unknown auth_strength %q", ErrInvalidIdentifier, string(a.AuthStrength))
	}
	switch a.Type {
	case ActorHuman:
		if a.SessionID == "" {
			return fmt.Errorf("%w: human actor requires a server-side session id", ErrInvalidIdentifier)
		}
		if a.AuthTime.IsZero() {
			return fmt.Errorf("%w: human actor requires auth_time for step-up evaluation", ErrMissingTimestamp)
		}
	case ActorWorkload, ActorAgent:
		if a.WorkloadAudience == "" {
			return fmt.Errorf("%w: workload actor requires an audience", ErrInvalidIdentifier)
		}
		if a.AuthStrength != AuthPhishingResistant {
			return fmt.Errorf("%w: workload actor requires workload-identity assurance, got %q",
				ErrInvalidIdentifier, string(a.AuthStrength))
		}
	}
	return nil
}

// Redacted returns a copy safe for logs: the display label is dropped and the
// session identifier is replaced by a short digest so that logs correlate
// without exposing session material.
func (a Actor) Redacted() Actor {
	r := a
	r.Display = ""
	r.SessionID = ""
	return r
}

// ---------------------------------------------------------------------------
// Instrument identity
// ---------------------------------------------------------------------------

// Instrument is the canonical internal instrument identity
// (16_MARKET_DATA_AND_VENUE_ADAPTERS.md §1). A venue symbol is never a globally
// unique identifier; it is a venue-scoped mapping held in VenueSymbol.
type Instrument struct {
	// ID is the platform instrument identifier.
	ID ID
	// MarketClass is the independently enabled market scope.
	MarketClass MarketClass
	// Base and Quote are the canonical currency/asset pair. For derivative
	// contracts they are set from the contract definition.
	Base  Currency
	Quote Currency
	// ContractDefinition is set for derivative/commodity instruments and
	// carries the canonical contract specification.
	ContractDefinition string
	// TradingStatus is the latest known platform status. UNKNOWN is not tradable.
	TradingStatus TradingStatus
	// StatusEffectiveAt is when TradingStatus took effect.
	StatusEffectiveAt Timestamp
	// MinQuantity and PriceIncrement are canonical increments. Venue adapters may
	// impose stricter values; the Risk Engine rejects when a required rule is
	// unknown.
	MinQuantity    Decimal
	PriceIncrement Decimal
	// TickSize is the absolute price movement unit for the instrument.
	TickSize Decimal
}

// Validate enforces required instrument fields.
func (i Instrument) Validate() error {
	if !i.ID.Valid() {
		return fmt.Errorf("%w: instrument id", ErrInvalidIdentifier)
	}
	if err := i.MarketClass.Validate(); err != nil {
		return err
	}
	if err := i.Base.ValidateCurrency(); err != nil {
		return fmt.Errorf("%w: base: %v", ErrInvalidIdentifier, err)
	}
	if err := i.Quote.ValidateCurrency(); err != nil {
		return fmt.Errorf("%w: quote: %v", ErrInvalidIdentifier, err)
	}
	if !i.TradingStatus.Known() {
		return fmt.Errorf("%w: unknown trading status %q", ErrInvalidIdentifier, string(i.TradingStatus))
	}
	// Precision metadata is required. A missing or non-positive price increment
	// means the instrument's venue rule is unknown, which is a deny condition for
	// risk-increasing activity (16_MARKET_DATA_AND_VENUE_ADAPTERS.md §5).
	if i.PriceIncrement.Sign() <= 0 {
		return fmt.Errorf("%w: price increment must be present and positive", ErrInvalidIdentifier)
	}
	return nil
}

// VenueSymbol is a venue-scoped symbol mapping. It is never used as a
// platform-wide instrument identity.
type VenueSymbol struct {
	InstrumentID ID     `json:"instrument_id"`
	VenueID      ID     `json:"venue_id"`
	Symbol       string `json:"symbol"`
	// MappingVersion records the version of this mapping so that a change is
	// versioned and audited rather than silently applied.
	MappingVersion int64     `json:"mapping_version"`
	EffectiveAt    Timestamp `json:"effective_at"`
}

// ---------------------------------------------------------------------------
// Order vocabulary
// ---------------------------------------------------------------------------

// Side is the direction of an order. Unknown values are unsupported and are
// never silently mapped to a default.
type Side string

const (
	SideBuy  Side = "BUY"
	SideSell Side = "SELL"
)

// Valid reports whether the side is a member of the closed set.
func (s Side) Valid() bool { return s == SideBuy || s == SideSell }

// Sign returns +1 for BUY and -1 for SELL, for exposure arithmetic.
func (s Side) Sign() int64 {
	if s == SideBuy {
		return 1
	}
	return -1
}

// Opposite returns the opposing side, used when a reduce-only order is
// synthesised for a risk-reducing action.
func (s Side) Opposite() Side {
	if s == SideBuy {
		return SideSell
	}
	return SideBuy
}

// OrderType is the canonical order type vocabulary.
type OrderType string

const (
	OrderMarket        OrderType = "MARKET"
	OrderLimit         OrderType = "LIMIT"
	OrderStop          OrderType = "STOP"
	OrderStopLimit     OrderType = "STOP_LIMIT"
	OrderPostOnly      OrderType = "POST_ONLY"
	OrderIOC           OrderType = "IOC"
	OrderFOK           OrderType = "FOK"
	OrderReduceOnly    OrderType = "REDUCE_ONLY"
	OrderClosePosition OrderType = "CLOSE_POSITION"
)

// OrderTypes returns the whole order-type vocabulary, in the declaration order.
//
// Exported so the Go-to-SQL parity test can compare this list against
// common.order_type without restating it. A hand-copied list in a test is a
// second copy of the vocabulary: adding an order type to the constants above and
// forgetting the test copy leaves the test passing against a vocabulary the
// service no longer implements, which is the drift the test exists to catch.
func OrderTypes() []OrderType {
	return []OrderType{
		OrderMarket, OrderLimit, OrderStop, OrderStopLimit, OrderPostOnly,
		OrderIOC, OrderFOK, OrderReduceOnly, OrderClosePosition,
	}
}

// Valid reports membership of the closed order-type set.
//
// Written in terms of OrderTypes rather than as a second literal list, so there
// is exactly one place that says what the vocabulary is.
func (o OrderType) Valid() bool {
	for _, t := range OrderTypes() {
		if o == t {
			return true
		}
	}
	return false
}

// RiskIncreasing reports whether submitting this order type can increase
// exposure. The Risk Engine applies its full mandatory control set only to
// risk-increasing commands; risk-reducing actions use the separately defined
// safe-reduction policy (17_CONFIGURATION_AND_RISK_POLICY.md §4).
func (o OrderType) RiskIncreasing() bool {
	switch o {
	case OrderReduceOnly, OrderClosePosition:
		return false
	}
	return true
}

// TimeInForce controls how long an order remains active.
type TimeInForce string

const (
	TIFGTC TimeInForce = "GTC"
	TIFDAY TimeInForce = "DAY"
	TIFIOC TimeInForce = "IOC"
	TIFFOK TimeInForce = "FOK"
	TIFGTX TimeInForce = "GTX"
)

// TimeInForces returns the whole time-in-force vocabulary, in declaration order.
// Exported for the same reason as OrderTypes: so the parity test compares this
// list against common.time_in_force rather than a hand-copied second copy.
func TimeInForces() []TimeInForce {
	return []TimeInForce{TIFGTC, TIFDAY, TIFIOC, TIFFOK, TIFGTX}
}

// Valid reports membership of the closed time-in-force set, in terms of the one
// list that defines the vocabulary.
func (t TimeInForce) Valid() bool {
	for _, v := range TimeInForces() {
		if t == v {
			return true
		}
	}
	return false
}
