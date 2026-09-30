package contracts

import (
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Exact base-10 decimal arithmetic.
//
// 03_CANONICAL_CONTRACTS.md: "No IEEE-754 floating point value may cross a
// financial contract." Every monetary and quantity value is therefore carried
// as an exact base-10 coefficient plus a signed scale, never as a float64.
// ---------------------------------------------------------------------------

var (
	// ErrInvalidDecimal indicates a malformed decimal literal.
	ErrInvalidDecimal = errors.New("contracts: invalid decimal literal")
	// ErrScaleOverflow indicates a loss of precision that was explicitly
	// rejected rather than silently rounded.
	ErrScaleOverflow = errors.New("contracts: decimal scale overflow")
)

// RoundingMode selects the rounding applied when a result must be reduced to a
// target scale. Canonical modes are half-away-from-zero and half-even
// (banker's rounding). Half-even is the default for money because it is
// unbiased across repeated aggregation; see ADR-026.
type RoundingMode int

const (
	// RoundHalfEven rounds to nearest, ties to the even digit. Default.
	RoundHalfEven RoundingMode = iota
	// RoundHalfUp rounds to nearest, ties away from zero.
	RoundHalfUp
	// RoundTruncate truncates toward zero.
	RoundTruncate
	// RoundFloor rounds toward negative infinity.
	RoundFloor
	// RoundCeil rounds toward positive infinity.
	RoundCeil
)

func (m RoundingMode) String() string {
	switch m {
	case RoundHalfEven:
		return "HALF_EVEN"
	case RoundHalfUp:
		return "HALF_UP"
	case RoundTruncate:
		return "TRUNCATE"
	case RoundFloor:
		return "FLOOR"
	case RoundCeil:
		return "CEIL"
	}
	return "UNKNOWN"
}

// ParseRoundingMode parses the canonical wire representation of a rounding mode.
func ParseRoundingMode(s string) (RoundingMode, error) {
	switch strings.ToUpper(s) {
	case "HALF_EVEN", "":
		return RoundHalfEven, nil
	case "HALF_UP":
		return RoundHalfUp, nil
	case "TRUNCATE":
		return RoundTruncate, nil
	case "FLOOR":
		return RoundFloor, nil
	case "CEIL":
		return RoundCeil, nil
	}
	return 0, fmt.Errorf("%w: unknown rounding mode %q", ErrInvalidDecimal, s)
}

// Decimal is an exact base-10 value represented as coefficient * 10^-scale.
// The zero value is a valid Decimal equal to 0 with scale 0.
//
// Decimal is immutable: every operation returns a new value and receivers are
// never modified. It is not safe to use Decimal as a map key or compare with
// ==; use Cmp and Equal for value semantics and String for a stable form.
type Decimal struct {
	coef  *big.Int
	scale int32
}

// DecimalScaleLimit bounds the scale so that pathological input cannot force
// unbounded big.Int allocation. 38 significant fractional digits far exceeds
// every venue and accounting requirement while keeping the attack surface small.
const DecimalScaleLimit int32 = 38

var (
	bigZero = big.NewInt(0)
	bigTen  = big.NewInt(10)
	bigOne  = big.NewInt(1)
)

// Zero returns the canonical zero Decimal.
func Zero() Decimal { return Decimal{coef: new(big.Int), scale: 0} }

// NewDecimal builds a Decimal from a coefficient and scale. It fails when the
// scale is outside the permitted range.
func NewDecimal(coef *big.Int, scale int32) (Decimal, error) {
	if scale < -DecimalScaleLimit || scale > DecimalScaleLimit {
		return Decimal{}, fmt.Errorf("%w: scale %d outside [-%d,%d]",
			ErrScaleOverflow, scale, DecimalScaleLimit, DecimalScaleLimit)
	}
	if coef == nil {
		coef = new(big.Int)
	}
	return Decimal{coef: new(big.Int).Set(coef), scale: scale}, nil
}

// ParseDecimal parses a strict base-10 decimal literal.
//
// Accepted grammar (canonical wire form):
//
//	[+-]? ( digits [ '.' digits? ] | '.' digits )
//
// Rejected: exponents ("1e-8"), hexadecimal, whitespace, thousands separators,
// "NaN", "Inf", and an empty string. A trailing fractional point ("1.") is
// accepted as scale 0; a leading point (".5") is accepted as 0.5. Exponents are
// rejected because they make the canonical representation non-unique, which
// would break audit record hashing and cross-language contract fixtures.
func ParseDecimal(s string) (Decimal, error) {
	if s == "" {
		return Decimal{}, fmt.Errorf("%w: empty", ErrInvalidDecimal)
	}
	neg := false
	i := 0
	switch s[0] {
	case '+':
		i = 1
	case '-':
		neg = true
		i = 1
	}
	intPart := ""
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		intPart += string(s[i])
		i++
	}
	fracPart := ""
	hasDot := false
	if i < len(s) && s[i] == '.' {
		hasDot = true
		i++
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			fracPart += string(s[i])
			i++
		}
	}
	if i != len(s) {
		return Decimal{}, fmt.Errorf("%w: %q has trailing or unsupported characters", ErrInvalidDecimal, s)
	}
	if intPart == "" && fracPart == "" {
		return Decimal{}, fmt.Errorf("%w: %q has no digits", ErrInvalidDecimal, s)
	}
	if !hasDot {
		fracPart = ""
	}
	digits := intPart + fracPart
	if len(digits) == 0 {
		return Decimal{}, fmt.Errorf("%w: %q has no digits", ErrInvalidDecimal, s)
	}
	// Reject a magnitude that cannot be represented in int32 scale terms.
	if int64(len(fracPart)) > int64(DecimalScaleLimit) {
		return Decimal{}, fmt.Errorf("%w: %q exceeds scale limit %d", ErrScaleOverflow, s, DecimalScaleLimit)
	}
	coef, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return Decimal{}, fmt.Errorf("%w: %q", ErrInvalidDecimal, s)
	}
	if neg {
		coef.Neg(coef)
	}
	// Normalise leading zeros in the integral part so that "007.50" and "7.5"
	// produce the identical canonical form. Trailing fractional zeros are NOT
	// stripped: scale is meaningful metadata that venues and policies compare.
	scale := int32(len(fracPart))
	return Decimal{coef: coef, scale: scale}, nil
}

// MustParseDecimal is ParseDecimal for static inputs. It panics on invalid
// input; use it only for literals known at authoring time.
func MustParseDecimal(s string) Decimal {
	d, err := ParseDecimal(s)
	if err != nil {
		panic(err)
	}
	return d
}

// Scale returns the number of fractional digits represented.
func (d Decimal) Scale() int32 { return d.scale }

// Coef returns a defensive copy of the unscaled coefficient.
func (d Decimal) Coef() *big.Int {
	if d.coef == nil {
		return new(big.Int)
	}
	return new(big.Int).Set(d.coef)
}

func (d Decimal) coeff() *big.Int {
	if d.coef == nil {
		return bigZero
	}
	return d.coef
}

// IsZero reports whether the value is exactly zero.
func (d Decimal) IsZero() bool { return d.coeff().Sign() == 0 }

// Sign returns -1, 0 or +1.
func (d Decimal) Sign() int { return d.coeff().Sign() }

// rescale returns an equivalent coefficient expressed at the target scale.
// It reports false when the requested scale exceeds the representable limit.
func (d Decimal) rescale(target int32) (*big.Int, bool) {
	if d.scale == target {
		return d.coeff(), true
	}
	diff := d.scale - target
	if diff < 0 {
		if -diff > DecimalScaleLimit {
			return nil, false
		}
		p := new(big.Int).Exp(bigTen, big.NewInt(int64(-diff)), nil)
		return new(big.Int).Mul(d.coeff(), p), true
	}
	if diff > DecimalScaleLimit {
		return nil, false
	}
	p := new(big.Int).Exp(bigTen, big.NewInt(int64(diff)), nil)
	return new(big.Int).Quo(d.coeff(), p), true
}

// align brings two decimals to a common scale and returns their coefficients.
// The common scale is bounded; overflow is reported rather than truncated.
func align(a, b Decimal) (ai, bi *big.Int, err error) {
	target := a.scale
	if b.scale > target {
		target = b.scale
	}
	if target < -DecimalScaleLimit || target > DecimalScaleLimit {
		return nil, nil, fmt.Errorf("%w: common scale %d", ErrScaleOverflow, target)
	}
	ai, ok := a.rescale(target)
	if !ok {
		return nil, nil, fmt.Errorf("%w: left operand", ErrScaleOverflow)
	}
	bi, ok = b.rescale(target)
	if !ok {
		return nil, nil, fmt.Errorf("%w: right operand", ErrScaleOverflow)
	}
	return ai, bi, nil
}

// Add returns a + b exactly. The result scale is the larger of the two input
// scales; no rounding is applied.
func (d Decimal) Add(o Decimal) (Decimal, error) {
	ai, bi, err := align(d, o)
	if err != nil {
		return Decimal{}, err
	}
	target := d.scale
	if o.scale > target {
		target = o.scale
	}
	return Decimal{coef: new(big.Int).Add(ai, bi), scale: target}, nil
}

// Sub returns d - o exactly.
func (d Decimal) Sub(o Decimal) (Decimal, error) {
	ai, bi, err := align(d, o)
	if err != nil {
		return Decimal{}, err
	}
	target := d.scale
	if o.scale > target {
		target = o.scale
	}
	return Decimal{coef: new(big.Int).Sub(ai, bi), scale: target}, nil
}

// Mul returns the exact product. The result scale is the sum of input scales,
// which is bounded by DecimalScaleLimit.
func (d Decimal) Mul(o Decimal) (Decimal, error) {
	scale := d.scale + o.scale
	if scale < -DecimalScaleLimit || scale > DecimalScaleLimit {
		return Decimal{}, fmt.Errorf("%w: product scale %d", ErrScaleOverflow, scale)
	}
	return Decimal{coef: new(big.Int).Mul(d.coeff(), o.coeff()), scale: scale}, nil
}

// Neg returns -d.
func (d Decimal) Neg() Decimal {
	return Decimal{coef: new(big.Int).Neg(d.coeff()), scale: d.scale}
}

// Abs returns |d|.
func (d Decimal) Abs() Decimal {
	return Decimal{coef: new(big.Int).Abs(d.coeff()), scale: d.scale}
}

// Cmp compares two decimals numerically: -1 if d < o, 0 if equal, +1 if d > o.
// Scale is not significant; "1.50" and "1.5" compare equal.
func (d Decimal) Cmp(o Decimal) int {
	ai, bi, err := align(d, o)
	if err != nil {
		// Alignment overflow means the magnitudes differ far beyond the
		// representable range. Compare signs and magnitudes deterministically.
		ds, os := d.Sign(), o.Sign()
		if ds != os {
			if ds < os {
				return -1
			}
			return 1
		}
		if ds == 0 {
			return 0
		}
		// Same non-zero sign: compare absolute coefficient magnitudes.
		return d.coeff().Cmp(o.coeff())
	}
	return ai.Cmp(bi)
}

// Equal reports numeric equality, ignoring scale.
func (d Decimal) Equal(o Decimal) bool { return d.Cmp(o) == 0 }

// String renders the canonical decimal literal. The form is unique for a given
// (coefficient, scale) pair, which makes it safe for audit record hashing and
// for cross-language fixture comparison.
//
// A negative scale denotes a magnitude in tens/hundreds (value = coef * 10^-scale
// with scale < 0) and is rendered by appending |scale| zeros.
func (d Decimal) String() string {
	c := d.coeff()
	neg := c.Sign() < 0
	abs := new(big.Int).Abs(c).String()
	switch {
	case d.scale == 0:
		if neg {
			return "-" + abs
		}
		return abs
	case d.scale < 0:
		if neg {
			return "-" + abs + strings.Repeat("0", int(-d.scale))
		}
		return abs + strings.Repeat("0", int(-d.scale))
	}
	// Pad the integral part to at least one digit.
	n := int(d.scale)
	if len(abs) <= n {
		abs = strings.Repeat("0", n-len(abs)+1) + abs
	}
	cut := len(abs) - n
	out := abs[:cut] + "." + abs[cut:]
	if neg {
		return "-" + out
	}
	return out
}

// MarshalText emits the canonical decimal string.
func (d Decimal) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// UnmarshalText parses a canonical decimal string with full validation.
func (d *Decimal) UnmarshalText(b []byte) error {
	p, err := ParseDecimal(string(b))
	if err != nil {
		return err
	}
	*d = p
	return nil
}

// Round reduces d to the requested scale using the supplied mode.
//
// A negative target scale rounds to tens, hundreds and so on, which venues
// occasionally require for block-quantity contracts.
func (d Decimal) Round(target int32, mode RoundingMode) (Decimal, error) {
	if target < -DecimalScaleLimit || target > DecimalScaleLimit {
		return Decimal{}, fmt.Errorf("%w: target scale %d", ErrScaleOverflow, target)
	}
	if d.scale == target {
		return d, nil
	}
	if d.scale < target {
		// Increasing scale is exact: append zeros.
		return d.rescaled(target)
	}
	// d.scale > target: divide by 10^(d.scale-target) applying the mode.
	shift := int64(d.scale - target)
	div := new(big.Int).Exp(bigTen, big.NewInt(shift), nil)
	q, r := new(big.Int).QuoRem(d.coeff(), div, new(big.Int))
	if r.Sign() == 0 {
		return Decimal{coef: q, scale: target}, nil
	}
	switch mode {
	case RoundTruncate:
		// q already truncated toward zero.
	case RoundFloor:
		if d.Sign() < 0 {
			q.Sub(q, bigOne)
		}
	case RoundCeil:
		if d.Sign() > 0 {
			q.Add(q, bigOne)
		}
	case RoundHalfUp:
		twice := new(big.Int).Abs(r)
		twice.Mul(twice, bigTwo)
		if twice.Cmp(div) >= 0 {
			if d.Sign() < 0 {
				q.Sub(q, bigOne)
			} else {
				q.Add(q, bigOne)
			}
		}
	case RoundHalfEven:
		twice := new(big.Int).Abs(r)
		twice.Mul(twice, bigTwo)
		switch twice.Cmp(div) {
		case 1:
			if d.Sign() < 0 {
				q.Sub(q, bigOne)
			} else {
				q.Add(q, bigOne)
			}
		case 0:
			// Exact tie: choose the even quotient.
			if q.Bit(0) == 1 {
				if d.Sign() < 0 {
					q.Sub(q, bigOne)
				} else {
					q.Add(q, bigOne)
				}
			}
		}
	default:
		return Decimal{}, fmt.Errorf("%w: unsupported rounding mode %d", ErrInvalidDecimal, int(mode))
	}
	return Decimal{coef: q, scale: target}, nil
}

var bigTwo = big.NewInt(2)

func (d Decimal) rescaled(target int32) (Decimal, error) {
	c, ok := d.rescale(target)
	if !ok {
		return Decimal{}, fmt.Errorf("%w: target scale %d", ErrScaleOverflow, target)
	}
	return Decimal{coef: c, scale: target}, nil
}

// Quantize is Round with the canonical default mode (half-even). It is the
// operation used when adapting a value to venue price/quantity increments.
func (d Decimal) Quantize(target int32) (Decimal, error) {
	return d.Round(target, RoundHalfEven)
}

// ---------------------------------------------------------------------------
// Money
// ---------------------------------------------------------------------------

// Currency is an ISO 4217 alphabetic code, or an explicit configured asset code
// for digital assets (03_CANONICAL_CONTRACTS.md, "Money").
type Currency string

// FiatMinorUnit holds the ISO 4217 minor unit exponent for the currencies whose
// exponent is not 2. Codes not listed are conventionally 2 and are asserted by
// contract tests against this table's default.
var fiatMinorUnit = map[Currency]int32{
	"USD": 2, "EUR": 2, "GBP": 2, "JPY": 0, "KRW": 0,
	"BHD": 3, "KWD": 3, "OMR": 3, "JOD": 3, "TND": 3,
	"IDR": 2, "SGD": 2, "AUD": 2, "CAD": 2, "CHF": 2,
	"HKD": 2, "CNY": 2, "INR": 2, "MYR": 2, "PHP": 2,
	"THB": 2, "VND": 0, "TWD": 2, "NZD": 2, "SEK": 2, "NOK": 2, "DKK": 2,
	"CLP": 0, "COP": 0, "ARS": 0, "HUF": 2, "CZK": 2, "PLN": 2, "RON": 2,
	"TRY": 2, "ZAR": 2, "BRL": 2, "MXN": 2, "AED": 2, "SAR": 2,
}

// DefaultMinorUnit is the minor-unit exponent assumed for a currency that is not
// explicitly listed. 2 is the ISO 4217 default. A digital asset that does not
// follow the 2-exponent convention must be declared as a crypto asset with an
// explicit scale, never silently accepted at the fiat default.
const DefaultMinorUnit int32 = 2

// IsFiat reports whether the code is a recognised ISO 4217 code.
func (c Currency) IsFiat() bool {
	_, ok := fiatMinorUnit[c]
	return ok
}

// IsDigitalAsset reports whether the code is a configured digital asset code.
// Digital asset codes are lowercase-or-uppercase alphabetic identifiers that are
// not ISO 4217 codes; they must be explicitly enabled in configuration because
// their precision is venue- and asset-specific.
func (c Currency) IsDigitalAsset() bool {
	if c == "" {
		return false
	}
	return !c.IsFiat()
}

// MinorUnit returns the ISO 4217 minor unit exponent. It returns an error for
// a digital asset, whose scale must come from explicit configuration or venue
// rules rather than from this table.
func (c Currency) MinorUnit() (int32, error) {
	if v, ok := fiatMinorUnit[c]; ok {
		return v, nil
	}
	return 0, fmt.Errorf("%w: %q has no ISO 4217 minor unit; declare an explicit scale", ErrInvalidDecimal, string(c))
}

// MinCurrencyLen and MaxCurrencyLen bound a currency code. ISO 4217 fiat codes
// are exactly three uppercase letters; longer codes are permitted only as
// explicitly configured digital-asset codes and are *not* evidence that the
// asset is permitted for any account, venue or jurisdiction.
const (
	MinCurrencyLen = 3
	MaxCurrencyLen = 12
)

// ValidateCurrency checks the shape of a currency code: 3 to 12 characters of
// uppercase ASCII letters, digits and underscore.
//
// It performs no eligibility inference and is not an ISO 4217 membership test;
// a well-formed code is not evidence that the currency is permitted.
func (c Currency) ValidateCurrency() error {
	if len(c) < MinCurrencyLen || len(c) > MaxCurrencyLen {
		return fmt.Errorf("%w: currency %q length outside [%d,%d]",
			ErrInvalidDecimal, string(c), MinCurrencyLen, MaxCurrencyLen)
	}
	for i := 0; i < len(c); i++ {
		ch := c[i]
		ok := (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_'
		if !ok {
			return fmt.Errorf("%w: currency %q has character %q outside [A-Z0-9_]",
				ErrInvalidDecimal, string(c), string(ch))
		}
	}
	return nil
}

// Money is the canonical monetary value: an exact decimal amount paired with its
// currency. It is the only representation permitted across a financial contract.
type Money struct {
	Currency Currency
	Amount   Decimal
}

// NewMoney builds a Money and validates the currency code.
func NewMoney(c Currency, amount Decimal) (Money, error) {
	if err := c.ValidateCurrency(); err != nil {
		return Money{}, err
	}
	return Money{Currency: c, Amount: amount}, nil
}

// MustParseMoney parses "<amount> <currency>", the canonical log/wire form.
func MustParseMoney(s string) Money {
	parts := strings.Fields(s)
	if len(parts) != 2 {
		panic("contracts: malformed money literal")
	}
	m, err := NewMoney(Currency(parts[1]), MustParseDecimal(parts[0]))
	if err != nil {
		panic(err)
	}
	return m
}

// IsZero reports a zero amount. Zero-amount money is still valid (used for
// realized-P&L offset entries) and is distinct from an absent value.
func (m Money) IsZero() bool { return m.Amount.IsZero() }

// String renders the canonical "<amount> <currency>" form.
func (m Money) String() string { return m.Amount.String() + " " + string(m.Currency) }

// Add returns m + o. Currency mismatch is an error rather than an implicit
// conversion: the platform never applies an FX rate to produce an authoritative
// financial fact. FX conversion is a separately governed, separately audited
// operation.
func (m Money) Add(o Money) (Money, error) {
	if m.Currency != o.Currency {
		return Money{}, fmt.Errorf("%w: currency mismatch %s vs %s", ErrInvalidDecimal, m.Currency, o.Currency)
	}
	sum, err := m.Amount.Add(o.Amount)
	if err != nil {
		return Money{}, err
	}
	return Money{Currency: m.Currency, Amount: sum}, nil
}

// Sub returns m - o with the same currency-mismatch rule as Add.
func (m Money) Sub(o Money) (Money, error) {
	if m.Currency != o.Currency {
		return Money{}, fmt.Errorf("%w: currency mismatch %s vs %s", ErrInvalidDecimal, m.Currency, o.Currency)
	}
	d, err := m.Amount.Sub(o.Amount)
	if err != nil {
		return Money{}, err
	}
	return Money{Currency: m.Currency, Amount: d}, nil
}

// Neg returns -m.
func (m Money) Neg() Money { return Money{Currency: m.Currency, Amount: m.Amount.Neg()} }

// Cmp compares two monetary values. Values in different currencies are not
// comparable; the second return value reports that.
func (m Money) Cmp(o Money) (int, error) {
	if m.Currency != o.Currency {
		return 0, fmt.Errorf("%w: cannot compare %s with %s", ErrInvalidDecimal, m.Currency, o.Currency)
	}
	return m.Amount.Cmp(o.Amount), nil
}

// QuantizeToMinorUnit reduces the amount to the ISO 4217 minor unit for a fiat
// currency using half-even rounding. Digital assets are rejected: their scale
// comes from venue or configuration rules, not from ISO 4217.
func (m Money) QuantizeToMinorUnit() (Money, error) {
	mu, err := m.Currency.MinorUnit()
	if err != nil {
		return Money{}, err
	}
	a, err := m.Amount.Quantize(mu)
	if err != nil {
		return Money{}, err
	}
	return Money{Currency: m.Currency, Amount: a}, nil
}

// ParseMoneyJSON decodes the canonical JSON monetary form:
// {"currency":"USD","amount":"1234.56"}. The amount is a JSON string, never a
// JSON number, so that no JSON decoder can introduce floating point into an
// authoritative financial value.
func ParseMoneyJSON(currency, amount string) (Money, error) {
	return NewMoney(Currency(currency), MustParseDecimal(amount))
}

// MoneyValue is the wire representation of Money used by every JSON contract.
type MoneyValue struct {
	Currency string `json:"currency"`
	Amount   string `json:"amount"`
}

// Wire converts to the JSON wire form.
func (m Money) Wire() MoneyValue {
	return MoneyValue{Currency: string(m.Currency), Amount: m.Amount.String()}
}

// FromWire validates and converts a JSON wire value. The amount is parsed with
// full decimal validation so that "12.3.4", "1e5" and "" are rejected.
func (v MoneyValue) FromWire() (Money, error) {
	if err := ValidateDecimalString(v.Amount); err != nil {
		return Money{}, err
	}
	amt, err := ParseDecimal(v.Amount)
	if err != nil {
		return Money{}, err
	}
	return NewMoney(Currency(v.Currency), amt)
}

// ValidateDecimalString performs strict canonical-literal validation without
// constructing a Decimal, for early rejection at the decoding boundary.
func ValidateDecimalString(s string) error {
	_, err := ParseDecimal(s)
	return err
}

// FormatDecimal renders a Decimal into an integer/fraction split for locales
// that require grouping (24_ENTERPRISE_RELEASE_STANDARD.md §2, i18n). Canonical
// storage and wire representation never use grouping separators.
//
// decimalSep is inserted before the fraction. groupSep is inserted every
// groupSize integral digits counted from the right. Passing an empty groupSep
// disables grouping. The canonical value is never modified.
func (d Decimal) FormatDecimal(decimalSep, groupSep string, groupSize int) string {
	s := d.String()
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	intPart, fracPart := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, fracPart = s[:i], s[i+1:]
	}
	var b strings.Builder
	if groupSep == "" || groupSize <= 0 {
		b.WriteString(intPart)
	} else {
		for i, ch := range intPart {
			if i > 0 && (len(intPart)-i)%groupSize == 0 {
				b.WriteString(groupSep)
			}
			b.WriteRune(ch)
		}
	}
	out := b.String()
	if fracPart != "" {
		out += decimalSep + fracPart
	}
	if neg {
		return "-" + out
	}
	return out
}

// ParseNonNegativeInt parses a strictly non-negative decimal integer, used for
// venue increments, lot sizes and similar integer-valued contract metadata.
func ParseNonNegativeInt(s string) (int64, error) {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrInvalidDecimal, s)
	}
	if v < 0 {
		return 0, fmt.Errorf("%w: %q is negative", ErrInvalidDecimal, s)
	}
	return v, nil
}
