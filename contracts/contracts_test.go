package contracts_test

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/aitc/trade/contracts"
)

// ---------------------------------------------------------------------------
// 03_CANONICAL_CONTRACTS.md: identifier format.
// ---------------------------------------------------------------------------

func TestIDFormatIsTypedPrefixPlusLowercaseCrockfordBase32(t *testing.T) {
	t.Parallel()
	for _, et := range []contracts.EntityType{
		contracts.EntityOrder, contracts.EntityEvent, contracts.EntityCommand,
		contracts.EntityStrategy, contracts.EntityModel, contracts.EntityRun,
		contracts.EntityPosition, contracts.EntityLedgerEntry,
	} {
		et := et
		t.Run(string(et), func(t *testing.T) {
			t.Parallel()
			id, err := contracts.NewID(et)
			if err != nil {
				t.Fatalf("NewID: %v", err)
			}
			s := id.String()
			if !strings.HasPrefix(s, string(et)+"_") {
				t.Fatalf("id %q missing typed prefix %q", s, et+"_")
			}
			payload := strings.TrimPrefix(s, string(et)+"_")
			if len(payload) != contracts.IDLength {
				t.Fatalf("payload %q length %d, want %d", payload, len(payload), contracts.IDLength)
			}
			// Payload must be lowercase Crockford Base32: no I, L, O, U.
			for _, ch := range payload {
				if strings.ContainsRune("ILOU", ch) {
					t.Fatalf("payload %q contains excluded Crockford character %q", payload, ch)
				}
				if ch >= 'A' && ch <= 'Z' {
					t.Fatalf("payload %q is not lowercase", payload)
				}
			}
			parsed, err := contracts.ParseID(s)
			if err != nil {
				t.Fatalf("ParseID(%q): %v", s, err)
			}
			if parsed.String() != s {
				t.Fatalf("round-trip changed id: %q -> %q", s, parsed.String())
			}
			if parsed.Entity != et {
				t.Fatalf("entity mismatch: got %q want %q", parsed.Entity, et)
			}
		})
	}
}

func TestIDUniquenessAtScale(t *testing.T) {
	t.Parallel()
	// 100 bits of entropy. A collision in 20k draws is astronomically unlikely,
	// and a collision would silently merge two authoritative records, so the
	// property is asserted rather than assumed.
	const n = 20000
	seen := make(map[contracts.ID]struct{}, n)
	for i := 0; i < n; i++ {
		id, err := contracts.NewID(contracts.EntityOrder)
		if err != nil {
			t.Fatalf("NewID: %v", err)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate identifier %q after %d draws", id, i)
		}
		seen[id] = struct{}{}
	}
}

func TestParseIDRejectsMalformedInput(t *testing.T) {
	t.Parallel()
	// Unknown entity types, wrong case, wrong length, excluded Crockford
	// letters and missing separators are all rejected. None may be silently
	// normalised into a valid identifier.
	bad := []string{
		"",
		"ord",
		"ord_",
		"ord_0123456789abcdefghj",       // 19 payload chars
		"ord_0123456789abcdefghjkmm",    // 21 payload chars
		"ord_0123456789ABCDEFGHJKM",     // uppercase payload
		"ord_0123456789abcdefghj#",      // character outside Crockford Base32
		"ORD_0123456789abcdefghjk",      // uppercase prefix
		"xyz_0123456789abcdefghjk",      // unknown prefix
		"ord-0123456789abcdefghjk",      // wrong separator
		"ord 0123456789abcdefghjk",      // space
		"ord_0123456789abcdefghjk\n",    // newline
		"ord_0123456789abcdefghjkextra", // over-length payload
		"ord0123456789abcdefghjk",       // no separator
	}
	for _, s := range bad {
		s := s
		t.Run(s, func(t *testing.T) {
			t.Parallel()
			if _, err := contracts.ParseID(s); err == nil {
				t.Fatalf("ParseID(%q) accepted an invalid identifier", s)
			}
		})
	}
}

func TestParseIDAppliesCrockfordDecodeTolerance(t *testing.T) {
	t.Parallel()
	// Crockford Base32 tolerates the visually ambiguous letters I, L, O and U on
	// decode. This is a decode convenience only: NewID never emits them, and a
	// tolerant decode still yields the canonical 20-character form.
	for _, s := range []string{
		"ord_0123456789abcdefghjk",
		"ord_0123456789abcdefghi1",
		"ord_0123456789abcdefgh0k",
	} {
		id, err := contracts.ParseID(s)
		if err != nil {
			t.Fatalf("ParseID(%q): %v", s, err)
		}
		if id.String() != s {
			t.Fatalf("tolerant decode changed the identifier: %q -> %q", s, id.String())
		}
	}
	// The excluded letters are still not encodable.
	raw := idPayloadOf(contracts.MustID(contracts.EntityOrder))
	for _, ch := range raw {
		if strings.ContainsRune("ILOUilou", ch) {
			t.Fatalf("NewID emitted the excluded Crockford character %q", ch)
		}
	}
}

func idPayloadOf(id contracts.ID) string {
	s := id.String()
	return s[len("ord_"):]
}

func TestNewIDRejectsUnknownEntityType(t *testing.T) {
	t.Parallel()
	if _, err := contracts.NewID(contracts.EntityType("zzz")); err == nil {
		t.Fatal("NewID accepted an unknown entity type; a typo must fail loudly")
	}
}

func TestIDMarshalsAsBareString(t *testing.T) {
	t.Parallel()
	id := contracts.MustID(contracts.EntityOrder)
	b, err := json.Marshal(id)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.HasPrefix(string(b), `"ord_`) {
		t.Fatalf("identifier did not marshal as a bare string: %s", b)
	}
	var back contracts.ID
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.String() != id.String() {
		t.Fatalf("round-trip mismatch %q != %q", back, id)
	}
}

func TestZeroIDIsInvalid(t *testing.T) {
	t.Parallel()
	var zero contracts.ID
	if zero.Valid() {
		t.Fatal("zero identifier reported as valid")
	}
	if !zero.IsZero() {
		t.Fatal("zero identifier not reported as zero")
	}
	if _, err := zero.MarshalText(); err == nil {
		t.Fatal("marshalling a zero identifier must fail rather than emit an empty identifier")
	}
}

// ---------------------------------------------------------------------------
// 03_CANONICAL_CONTRACTS.md: money and quantity.
// ---------------------------------------------------------------------------

func TestDecimalParseAcceptsCanonicalForms(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in    string
		scale int32
		text  string
	}{
		{"0", 0, "0"},
		{"1", 0, "1"},
		{"-1", 0, "-1"},
		{"1.5", 1, "1.5"},
		{"+1.5", 1, "1.5"},
		{"1.", 0, "1"},
		{".5", 1, "0.5"},
		{"-0.00000001", 8, "-0.00000001"},
		{"1.50", 2, "1.50"}, // trailing zeros are meaningful scale metadata
		{"007.5", 1, "7.5"}, // leading zeros are normalised
		{"123456789012345678901234567890.123", 3, "123456789012345678901234567890.123"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.in, func(t *testing.T) {
			t.Parallel()
			d, err := contracts.ParseDecimal(c.in)
			if err != nil {
				t.Fatalf("ParseDecimal(%q): %v", c.in, err)
			}
			if d.Scale() != c.scale {
				t.Fatalf("scale = %d, want %d", d.Scale(), c.scale)
			}
			if d.String() != c.text {
				t.Fatalf("String() = %q, want %q", d.String(), c.text)
			}
		})
	}
}

func TestDecimalParseRejectsNonCanonicalForms(t *testing.T) {
	t.Parallel()
	// Exponents, whitespace, separators, non-finite values and empty strings
	// are all rejected: the canonical representation must be unique so that
	// audit record hashing and cross-language fixtures agree.
	bad := []string{
		"", " ", "abc", "1e5", "1E5", "1.5e-3", "0x10", "1,000",
		"1 2", "1.2.3", "+", "-", ".", "..", "1.2.3.4", "NaN", "Inf",
		"1_000", "1.5 ", " 1.5", "١٢٣", "1.5;drop",
	}
	for _, s := range bad {
		s := s
		t.Run(s, func(t *testing.T) {
			t.Parallel()
			if d, err := contracts.ParseDecimal(s); err == nil {
				t.Fatalf("ParseDecimal(%q) accepted, produced %q", s, d.String())
			}
		})
	}
}

func TestDecimalArithmeticIsExact(t *testing.T) {
	t.Parallel()
	// Values that are inexact in binary floating point. The result must be
	// mathematically exact, proving no IEEE-754 value crossed the contract.
	a := contracts.MustParseDecimal("0.1")
	b := contracts.MustParseDecimal("0.2")
	sum, err := a.Add(b)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if sum.String() != "0.3" {
		t.Fatalf("0.1 + 0.2 = %q, want 0.3 (float64 would yield 0.30000000000000004)", sum.String())
	}
	tenth := contracts.MustParseDecimal("0.1")
	acc := contracts.Zero()
	for i := 0; i < 10; i++ {
		acc, err = acc.Add(tenth)
		if err != nil {
			t.Fatalf("accumulate: %v", err)
		}
	}
	if acc.String() != "1.0" {
		t.Fatalf("ten times 0.1 = %q, want 1.0", acc.String())
	}
	// Subtraction across differing scales must not lose the fractional part.
	one := contracts.MustParseDecimal("1")
	half := contracts.MustParseDecimal("0.5")
	d, err := one.Sub(half)
	if err != nil {
		t.Fatalf("Sub: %v", err)
	}
	if d.String() != "0.5" {
		t.Fatalf("1 - 0.5 = %q, want 0.5", d.String())
	}
	// Multiplication keeps exact scale.
	p, err := contracts.MustParseDecimal("1.25").Mul(contracts.MustParseDecimal("4"))
	if err != nil {
		t.Fatalf("Mul: %v", err)
	}
	if p.String() != "5.00" {
		t.Fatalf("1.25 * 4 = %q, want 5.00", p.String())
	}
}

func TestDecimalCompareIgnoresScale(t *testing.T) {
	t.Parallel()
	cases := []struct {
		a, b string
		want int
	}{
		{"1.50", "1.5", 0},
		{"1", "1.000", 0},
		{"0", "-0", 0},
		{"1", "2", -1},
		{"-1", "-2", 1},
		{"-2", "-1", -1},
		{"100000000000000000000", "99999999999999999999", 1},
	}
	for _, c := range cases {
		c := c
		t.Run(c.a+"|"+c.b, func(t *testing.T) {
			t.Parallel()
			got := contracts.MustParseDecimal(c.a).Cmp(contracts.MustParseDecimal(c.b))
			if got != c.want {
				t.Fatalf("cmp = %d, want %d", got, c.want)
			}
		})
	}
}

func TestDecimalRoundingModes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in     string
		scale  int32
		mode   contracts.RoundingMode
		expect string
	}{
		{"1.005", 2, contracts.RoundHalfEven, "1.00"}, // tie -> even
		{"1.015", 2, contracts.RoundHalfEven, "1.02"}, // tie -> even
		{"1.025", 2, contracts.RoundHalfEven, "1.02"},
		{"1.035", 2, contracts.RoundHalfEven, "1.04"},
		{"1.005", 2, contracts.RoundHalfUp, "1.01"},
		{"-1.005", 2, contracts.RoundHalfUp, "-1.01"},
		{"1.005", 2, contracts.RoundHalfEven, "1.00"},
		{"1.009", 2, contracts.RoundTruncate, "1.00"},
		{"-1.009", 2, contracts.RoundTruncate, "-1.00"},
		{"-1.001", 2, contracts.RoundFloor, "-1.01"},
		{"1.001", 2, contracts.RoundCeil, "1.01"},
		{"-1.001", 2, contracts.RoundCeil, "-1.00"},
		{"2.5", 0, contracts.RoundHalfEven, "2"},
		{"3.5", 0, contracts.RoundHalfEven, "4"},
		{"123.45", 1, contracts.RoundHalfEven, "123.4"},
		{"0.00000001", 2, contracts.RoundHalfEven, "0.00"},
		{"1234", -2, contracts.RoundHalfEven, "1200"},
		{"1250", -2, contracts.RoundHalfEven, "1200"},
		{"1350", -2, contracts.RoundHalfEven, "1400"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.in+"/"+c.mode.String(), func(t *testing.T) {
			t.Parallel()
			d := contracts.MustParseDecimal(c.in)
			got, err := d.Round(c.scale, c.mode)
			if err != nil {
				t.Fatalf("Round: %v", err)
			}
			if got.String() != c.expect {
				t.Fatalf("Round(%s,%d,%s) = %q, want %q", c.in, c.scale, c.mode, got.String(), c.expect)
			}
		})
	}
}

func TestDecimalRoundingIsIdempotent(t *testing.T) {
	t.Parallel()
	// Once a value is at its target scale, rounding again must not change it.
	for _, s := range []string{"1.23", "-1.23", "0.00", "99.99"} {
		d := contracts.MustParseDecimal(s)
		once, err := d.Round(2, contracts.RoundHalfEven)
		if err != nil {
			t.Fatalf("Round: %v", err)
		}
		twice, err := once.Round(2, contracts.RoundHalfEven)
		if err != nil {
			t.Fatalf("Round: %v", err)
		}
		if once.String() != twice.String() {
			t.Fatalf("rounding not idempotent for %q: %q then %q", s, once, twice)
		}
	}
}

func TestDecimalScaleLimitIsEnforced(t *testing.T) {
	t.Parallel()
	long := "0." + strings.Repeat("1", 40)
	if _, err := contracts.ParseDecimal(long); err == nil {
		t.Fatal("a 40-digit scale must be rejected as beyond the representable limit")
	}
}

func TestMoneyCurrencyMismatchIsAnError(t *testing.T) {
	t.Parallel()
	usd, err := contracts.NewMoney("USD", contracts.MustParseDecimal("100.00"))
	if err != nil {
		t.Fatalf("NewMoney: %v", err)
	}
	eur, err := contracts.NewMoney("EUR", contracts.MustParseDecimal("90.00"))
	if err != nil {
		t.Fatalf("NewMoney: %v", err)
	}
	// The platform never applies an implicit FX rate to produce an authoritative
	// financial fact, so cross-currency addition must fail.
	if _, err := usd.Add(eur); err == nil {
		t.Fatal("adding USD to EUR must fail; no implicit FX conversion is permitted")
	}
	if _, err := usd.Sub(eur); err == nil {
		t.Fatal("subtracting EUR from USD must fail")
	}
	if _, err := usd.Cmp(eur); err == nil {
		t.Fatal("comparing USD with EUR must fail")
	}
	sum, err := usd.Add(usd)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if sum.String() != "200.00 USD" {
		t.Fatalf("sum = %q, want 200.00 USD", sum)
	}
}

func TestMoneyMinorUnits(t *testing.T) {
	t.Parallel()
	cases := []struct {
		cur   contracts.Currency
		minor int32
	}{
		{"USD", 2}, {"EUR", 2}, {"JPY", 0}, {"KRW", 0}, {"BHD", 3},
		{"KWD", 3}, {"IDR", 2}, {"VND", 0}, {"CLP", 0},
	}
	for _, c := range cases {
		got, err := c.cur.MinorUnit()
		if err != nil {
			t.Fatalf("%s.MinorUnit: %v", c.cur, err)
		}
		if got != c.minor {
			t.Fatalf("%s minor unit = %d, want %d", c.cur, got, c.minor)
		}
	}
	// A digital asset has no ISO 4217 minor unit; its scale must come from
	// explicit venue or configuration rules.
	if _, err := contracts.Currency("BTC").MinorUnit(); err == nil {
		t.Fatal("a digital asset must not report an ISO 4217 minor unit")
	}
	if !contracts.Currency("BTC").IsDigitalAsset() {
		t.Fatal("BTC should be classified as a digital asset")
	}
	if !contracts.Currency("USD").IsFiat() {
		t.Fatal("USD should be classified as fiat")
	}
}

func TestMoneyQuantizeToMinorUnit(t *testing.T) {
	t.Parallel()
	usd, err := contracts.NewMoney("USD", contracts.MustParseDecimal("1.005"))
	if err != nil {
		t.Fatalf("NewMoney: %v", err)
	}
	q, err := usd.QuantizeToMinorUnit()
	if err != nil {
		t.Fatalf("Quantize: %v", err)
	}
	if q.String() != "1.00 USD" {
		t.Fatalf("quantized = %q, want 1.00 USD", q.String())
	}
	jpy, err := contracts.NewMoney("JPY", contracts.MustParseDecimal("1234.56"))
	if err != nil {
		t.Fatalf("NewMoney: %v", err)
	}
	jq, err := jpy.QuantizeToMinorUnit()
	if err != nil {
		t.Fatalf("Quantize: %v", err)
	}
	if jq.String() != "1235 JPY" {
		t.Fatalf("quantized = %q, want 1235 JPY (JPY has no minor unit)", jq.String())
	}
	btc, err := contracts.NewMoney("BTC", contracts.MustParseDecimal("0.123456789012345678"))
	if err != nil {
		t.Fatalf("NewMoney: %v", err)
	}
	if _, err := btc.QuantizeToMinorUnit(); err == nil {
		t.Fatal("quantizing a digital asset to an ISO minor unit must fail closed")
	}
}

func TestMoneyWireFormUsesStringAmount(t *testing.T) {
	t.Parallel()
	// The amount must be a JSON string so that no JSON decoder can introduce a
	// float into an authoritative value.
	m, err := contracts.NewMoney("USD", contracts.MustParseDecimal("1234567890.12"))
	if err != nil {
		t.Fatalf("NewMoney: %v", err)
	}
	b, err := json.Marshal(m.Wire())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"amount":"1234567890.12"`) {
		t.Fatalf("amount must marshal as a quoted string, got %s", b)
	}
	// 1.2345678901234568e+09 would be the JSON number form; assert absence.
	if strings.Contains(string(b), "e+") {
		t.Fatalf("wire form leaked a float: %s", b)
	}
	var back contracts.MoneyValue
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	round, err := back.FromWire()
	if err != nil {
		t.Fatalf("FromWire: %v", err)
	}
	if round.String() != m.String() {
		t.Fatalf("round-trip changed value: %q -> %q", m, round)
	}
}

func TestMoneyWireRejectsMalformedAmount(t *testing.T) {
	t.Parallel()
	for _, amount := range []string{"", "abc", "1e5", "1.2.3", " 1.00", "NaN"} {
		v := contracts.MoneyValue{Currency: "USD", Amount: amount}
		if _, err := v.FromWire(); err == nil {
			t.Fatalf("malformed amount %q was accepted", amount)
		}
	}
}

func TestMoneyRejectsInvalidCurrencyCode(t *testing.T) {
	t.Parallel()
	// Shape rules: 3-12 characters of uppercase ASCII letters, digits and
	// underscore. A longer uppercase code such as "USDDD" is shape-valid but
	// is classified as a digital-asset code, never as fiat; ISO membership is
	// decided by the eligibility registry, not by this validator.
	for _, c := range []string{"", "U", "US", "usd", "US!", "12", "US D", "US$"} {
		if _, err := contracts.NewMoney(contracts.Currency(c), contracts.MustParseDecimal("1")); err == nil {
			t.Fatalf("invalid currency %q was accepted", c)
		}
	}
	for _, c := range []string{"USD", "JPY", "BHD", "IDR", "BTC", "USDT", "XBT_USD"} {
		if _, err := contracts.NewMoney(contracts.Currency(c), contracts.MustParseDecimal("1")); err != nil {
			t.Fatalf("valid currency %q rejected: %v", c, err)
		}
	}
	if contracts.Currency("USDDD").IsFiat() {
		t.Fatal("a non-ISO code must not be reported as fiat")
	}
}

func TestDecimalLargeMagnitudeIsExact(t *testing.T) {
	t.Parallel()
	// A magnitude far beyond float64's 2^53 exact-integer range.
	big1, err := contracts.ParseDecimal("9007199254740993") // 2^53+1
	if err != nil {
		t.Fatalf("ParseDecimal: %v", err)
	}
	one := contracts.MustParseDecimal("1")
	got, err := big1.Add(one)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got.String() != "9007199254740994" {
		t.Fatalf("2^53+1 + 1 = %q; float64 would lose the low bit", got.String())
	}
	// Same value must compare exactly against a big.Int-computed reference.
	ref := new(big.Int).Add(big1.Coef(), one.Coef())
	if got.Coef().Cmp(ref) != 0 {
		t.Fatalf("coefficient mismatch against big.Int reference")
	}
}

func TestFormatDecimalDoesNotChangeCanonicalValue(t *testing.T) {
	t.Parallel()
	// Locale formatting is presentation only. The canonical string is unchanged.
	d := contracts.MustParseDecimal("-1234567.8900")
	// en-US grouping with a full stop decimal separator.
	if got := d.FormatDecimal(".", ",", 3); got != "-1,234,567.8900" {
		t.Fatalf("en-US formatted = %q, want -1,234,567.8900", got)
	}
	// de-DE grouping with a comma decimal separator.
	if got := d.FormatDecimal(",", ".", 3); got != "-1.234.567,8900" {
		t.Fatalf("de-DE formatted = %q, want -1.234.567,8900", got)
	}
	// Grouping disabled must reproduce the canonical form.
	if got := d.FormatDecimal(".", "", 3); got != "-1234567.8900" {
		t.Fatalf("ungrouped formatted = %q", got)
	}
	if d.String() != "-1234567.8900" {
		t.Fatalf("formatting mutated the canonical value: %q", d.String())
	}
	reparsed, err := contracts.ParseDecimal(d.String())
	if err != nil {
		t.Fatalf("canonical value no longer parses: %v", err)
	}
	if !reparsed.Equal(d) {
		t.Fatal("locale round-trip changed the value")
	}
}
