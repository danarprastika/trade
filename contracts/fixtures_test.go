package contracts_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aitc/trade/contracts"
)

// The fixtures file is the shared contract between Go, TypeScript and Python
// (02_POLYGLOT_ENGINEERING_STANDARD.md §9: "A component cannot promote if its
// contract fixtures disagree with the canonical contract repository").
// These tests bind the Go implementation to that file so drift is a build
// failure rather than a production incident.

type fixtures struct {
	Version     string `json:"version"`
	Identifiers struct {
		Valid   []string `json:"valid"`
		Invalid []struct {
			Value  string `json:"value"`
			Reason string `json:"reason"`
		} `json:"invalid"`
		CrockfordDecodeTolerance []struct {
			Value  string `json:"value"`
			Reason string `json:"reason"`
		} `json:"crockfordDecodeTolerance"`
		CrockfordEncodeExclusions []string `json:"crockfordEncodeExclusions"`
	} `json:"identifiers"`
	Decimals struct {
		Canonical []struct {
			Input     string `json:"input"`
			Scale     int32  `json:"scale"`
			Canonical string `json:"canonical"`
		} `json:"canonical"`
		Rejected []string `json:"rejected"`
	} `json:"decimals"`
	Rounding []struct {
		Input    string `json:"input"`
		Scale    int32  `json:"scale"`
		Mode     string `json:"mode"`
		Expected string `json:"expected"`
	} `json:"rounding"`
	Arithmetic []struct {
		Op       string `json:"op"`
		A        string `json:"a"`
		B        string `json:"b"`
		Expected string `json:"expected"`
	} `json:"arithmetic"`
	Money struct {
		MinorUnits []struct {
			Currency string `json:"currency"`
			Minor    int32  `json:"minor"`
		} `json:"minorUnits"`
		DigitalAssetsWithoutIsoMinorUnit []string `json:"digitalAssetsWithoutIsoMinorUnit"`
		WireExamples                     []struct {
			Currency string `json:"currency"`
			Amount   string `json:"amount"`
		} `json:"wireExamples"`
	} `json:"money"`
	Timestamps struct {
		Canonical []struct {
			Input    string `json:"input"`
			UnixNano string `json:"unixNano"`
		} `json:"canonical"`
		NonUtcNormalised []struct {
			Input     string `json:"input"`
			Canonical string `json:"canonical"`
		} `json:"nonUtcNormalised"`
		Rejected []string `json:"rejected"`
	} `json:"timestamps"`
	ErrorTaxonomy struct {
		HTTPMapping    map[string]int `json:"httpMapping"`
		Retryable      []string       `json:"retryable"`
		NeverRetryable []string       `json:"neverRetryable"`
	} `json:"errorTaxonomy"`
	Limits struct {
		MaxRequestBodyBytes          int `json:"maxRequestBodyBytes"`
		MaxCommandPayloadBytes       int `json:"maxCommandPayloadBytes"`
		DefaultPageSize              int `json:"defaultPageSize"`
		MaxPageSize                  int `json:"maxPageSize"`
		ReadRequestsPerMinute        int `json:"readRequestsPerMinute"`
		PrivilegedMutationsPerMinute int `json:"privilegedMutationsPerMinute"`
	} `json:"limits"`
	Vocabularies map[string][]string `json:"vocabularies"`
}

func loadFixtures(t *testing.T) *fixtures {
	t.Helper()
	p := filepath.Join("testdata", "canonical_fixtures.json")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	var f fixtures
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parse fixtures: %v", err)
	}
	if f.Version != string(contracts.CurrentSchemaVersion) {
		t.Fatalf("fixture version %q does not match the emitted schema version %q",
			f.Version, contracts.CurrentSchemaVersion)
	}
	return &f
}

func TestFixturesIdentifiers(t *testing.T) {
	t.Parallel()
	f := loadFixtures(t)
	if len(f.Identifiers.Valid) == 0 {
		t.Fatal("fixture file declares no valid identifiers")
	}
	for _, s := range f.Identifiers.Valid {
		if _, err := contracts.ParseID(s); err != nil {
			t.Errorf("fixture valid identifier %q rejected by Go: %v", s, err)
		}
	}
	for _, c := range f.Identifiers.Invalid {
		if _, err := contracts.ParseID(c.Value); err == nil {
			t.Errorf("fixture invalid identifier %q (%s) was accepted by Go", c.Value, c.Reason)
		}
	}
	for _, c := range f.Identifiers.CrockfordDecodeTolerance {
		if _, err := contracts.ParseID(c.Value); err != nil {
			t.Errorf("Crockford decode tolerance %q (%s) rejected: %v", c.Value, c.Reason, err)
		}
	}
	// Go's encoder must never emit an excluded character.
	excluded := map[rune]bool{}
	for _, ch := range f.Identifiers.CrockfordEncodeExclusions {
		for _, r := range ch {
			excluded[r] = true
		}
	}
	for i := 0; i < 5000; i++ {
		id := contracts.MustID(contracts.EntityOrder)
		for _, r := range idPayloadOf(id) {
			if excluded[r] {
				t.Fatalf("Go emitted the excluded Crockford character %q in %q", r, id)
			}
		}
	}
}

func TestFixturesDecimals(t *testing.T) {
	t.Parallel()
	f := loadFixtures(t)
	for _, c := range f.Decimals.Canonical {
		d, err := contracts.ParseDecimal(c.Input)
		if err != nil {
			t.Errorf("fixture decimal %q rejected: %v", c.Input, err)
			continue
		}
		if d.Scale() != c.Scale {
			t.Errorf("fixture decimal %q: scale = %d, want %d", c.Input, d.Scale(), c.Scale)
		}
		if d.String() != c.Canonical {
			t.Errorf("fixture decimal %q: canonical = %q, want %q", c.Input, d.String(), c.Canonical)
		}
	}
	for _, s := range f.Decimals.Rejected {
		if d, err := contracts.ParseDecimal(s); err == nil {
			t.Errorf("fixture rejected decimal %q was accepted as %q", s, d.String())
		}
	}
}

func TestFixturesRounding(t *testing.T) {
	t.Parallel()
	f := loadFixtures(t)
	for _, c := range f.Rounding {
		mode, err := contracts.ParseRoundingMode(c.Mode)
		if err != nil {
			t.Errorf("fixture rounding mode %q unknown to Go: %v", c.Mode, err)
			continue
		}
		got, err := contracts.MustParseDecimal(c.Input).Round(c.Scale, mode)
		if err != nil {
			t.Errorf("fixture rounding %s/%d/%s failed: %v", c.Input, c.Scale, c.Mode, err)
			continue
		}
		if got.String() != c.Expected {
			t.Errorf("fixture rounding %s -> scale %d mode %s: got %q, want %q",
				c.Input, c.Scale, c.Mode, got.String(), c.Expected)
		}
	}
}

func TestFixturesArithmetic(t *testing.T) {
	t.Parallel()
	f := loadFixtures(t)
	for _, c := range f.Arithmetic {
		a := contracts.MustParseDecimal(c.A)
		b := contracts.MustParseDecimal(c.B)
		var got contracts.Decimal
		var err error
		switch c.Op {
		case "add":
			got, err = a.Add(b)
		case "sub":
			got, err = a.Sub(b)
		case "mul":
			got, err = a.Mul(b)
		default:
			t.Fatalf("fixture declares an unknown operation %q", c.Op)
		}
		if err != nil {
			t.Errorf("fixture arithmetic %s %s %s: %v", c.Op, c.A, c.B, err)
			continue
		}
		if got.String() != c.Expected {
			t.Errorf("fixture arithmetic %s(%s,%s) = %q, want %q", c.Op, c.A, c.B, got.String(), c.Expected)
		}
	}
}

func TestFixturesMoney(t *testing.T) {
	t.Parallel()
	f := loadFixtures(t)
	for _, c := range f.Money.MinorUnits {
		got, err := c2c(c.Currency).MinorUnit()
		if err != nil {
			t.Errorf("fixture currency %s: %v", c.Currency, err)
			continue
		}
		if got != c.Minor {
			t.Errorf("fixture currency %s minor unit = %d, want %d", c.Currency, got, c.Minor)
		}
	}
	for _, code := range f.Money.DigitalAssetsWithoutIsoMinorUnit {
		if _, err := c2c(code).MinorUnit(); err == nil {
			t.Errorf("digital asset %s reported an ISO 4217 minor unit", code)
		}
	}
	for _, c := range f.Money.WireExamples {
		w := contracts.MoneyValue{Currency: c.Currency, Amount: c.Amount}
		m, err := w.FromWire()
		if err != nil {
			t.Errorf("fixture money %s rejected: %v", c.Amount, err)
			continue
		}
		if m.Amount.String() != c.Amount {
			t.Errorf("fixture money %s round-tripped to %s", c.Amount, m.Amount.String())
		}
		if m.Currency != c2c(c.Currency) {
			t.Errorf("fixture money currency mismatch")
		}
		// Cross-currency arithmetic must fail.
		other, _ := contracts.NewMoney("CHF", contracts.MustParseDecimal("1.00"))
		if _, err := m.Add(other); err == nil {
			t.Errorf("cross-currency addition of %s succeeded; implicit FX is prohibited", c.Currency)
		}
	}
}

func c2c(s string) contracts.Currency { return contracts.Currency(s) }

func TestFixturesTimestamps(t *testing.T) {
	t.Parallel()
	f := loadFixtures(t)
	for _, c := range f.Timestamps.Canonical {
		ts, err := contracts.ParseTimestamp(c.Input)
		if err != nil {
			t.Errorf("fixture timestamp %q rejected: %v", c.Input, err)
			continue
		}
		if ts.String() != c.Input {
			t.Errorf("fixture timestamp %q canonicalised to %q", c.Input, ts.String())
		}
		var want int64
		if _, err := jsonNumberToInt64(c.UnixNano, &want); err != nil {
			t.Fatalf("bad fixture unixNano %q: %v", c.UnixNano, err)
		}
		if ts.UnixNano() != want {
			t.Errorf("fixture timestamp %q: unixNano = %d, want %d", c.Input, ts.UnixNano(), want)
		}
	}
	for _, c := range f.Timestamps.NonUtcNormalised {
		ts, err := contracts.ParseTimestamp(c.Input)
		if err != nil {
			t.Errorf("fixture offset timestamp %q rejected: %v", c.Input, err)
			continue
		}
		if ts.String() != c.Canonical {
			t.Errorf("fixture offset timestamp %q normalised to %q, want %q", c.Input, ts.String(), c.Canonical)
		}
	}
	for _, s := range f.Timestamps.Rejected {
		if _, err := contracts.ParseTimestamp(s); err == nil {
			t.Errorf("fixture rejected timestamp %q was accepted", s)
		}
	}
}

func jsonNumberToInt64(s string, out *int64) (bool, error) {
	var v int64
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return false, err
	}
	*out = v
	return true, nil
}

func TestFixturesErrorTaxonomy(t *testing.T) {
	t.Parallel()
	f := loadFixtures(t)
	if len(f.ErrorTaxonomy.HTTPMapping) != len(contracts.CanonicalErrorCodes()) {
		t.Fatalf("fixture taxonomy has %d members, Go has %d",
			len(f.ErrorTaxonomy.HTTPMapping), len(contracts.CanonicalErrorCodes()))
	}
	for name, wantStatus := range f.ErrorTaxonomy.HTTPMapping {
		code := contracts.ErrorCode(name)
		if !code.Known() {
			t.Errorf("fixture error code %q is unknown to Go", name)
			continue
		}
		if got := code.HTTPStatus(); got != wantStatus {
			t.Errorf("fixture error code %q: HTTP %d, Go says %d", name, wantStatus, got)
		}
	}
	for _, name := range f.ErrorTaxonomy.Retryable {
		if !contracts.ErrorCode(name).Retryable() {
			t.Errorf("fixture says %q is retryable; Go disagrees", name)
		}
	}
	for _, name := range f.ErrorTaxonomy.NeverRetryable {
		if contracts.ErrorCode(name).Retryable() {
			t.Errorf("fixture says %q is never retryable; Go disagrees", name)
		}
	}
}

func TestFixturesLimits(t *testing.T) {
	t.Parallel()
	f := loadFixtures(t)
	if f.Limits.MaxRequestBodyBytes != contracts.MaxRequestBodyBytes {
		t.Errorf("request body limit: fixture %d, Go %d", f.Limits.MaxRequestBodyBytes, contracts.MaxRequestBodyBytes)
	}
	if f.Limits.MaxCommandPayloadBytes != contracts.MaxCommandPayloadBytes {
		t.Errorf("command payload limit: fixture %d, Go %d", f.Limits.MaxCommandPayloadBytes, contracts.MaxCommandPayloadBytes)
	}
}

func TestFixturesVocabularies(t *testing.T) {
	t.Parallel()
	f := loadFixtures(t)

	envs := make([]string, 0, len(f.Vocabularies["environment"]))
	for _, e := range f.Vocabularies["environment"] {
		envs = append(envs, e)
		if !contracts.Environment(e).Known() {
			t.Errorf("fixture environment %q unknown to Go", e)
		}
	}
	// Promotion ladder order is normative; assert the fixture order matches.
	ladder := []contracts.Environment{
		contracts.EnvDev, contracts.EnvTest, contracts.EnvStaging,
		contracts.EnvPaper, contracts.EnvShadow, contracts.EnvLive,
	}
	if len(envs) != len(ladder) {
		t.Fatalf("environment ladder length mismatch: fixture %d, Go %d", len(envs), len(ladder))
	}
	for i := range ladder {
		if envs[i] != string(ladder[i]) {
			t.Errorf("environment ladder position %d: fixture %q, Go %q", i, envs[i], ladder[i])
		}
	}

	classes := make([]string, 0)
	for _, m := range f.Vocabularies["marketClass"] {
		classes = append(classes, m)
		if !contracts.MarketClass(m).Known() {
			t.Errorf("fixture market class %q unknown to Go", m)
		}
	}
	if len(classes) != len(contracts.MarketClasses()) {
		t.Errorf("market class count: fixture %d, Go %d", len(classes), len(contracts.MarketClasses()))
	}

	for _, a := range f.Vocabularies["actorType"] {
		if !contracts.ActorType(a).Known() {
			t.Errorf("fixture actor type %q unknown to Go", a)
		}
	}
	for _, a := range f.Vocabularies["authStrength"] {
		if !contracts.AuthStrength(a).Known() {
			t.Errorf("fixture auth strength %q unknown to Go", a)
		}
	}
	for _, s := range f.Vocabularies["side"] {
		if !contracts.Side(s).Valid() {
			t.Errorf("fixture side %q unknown to Go", s)
		}
	}
	for _, o := range f.Vocabularies["orderType"] {
		if !contracts.OrderType(o).Valid() {
			t.Errorf("fixture order type %q unknown to Go", o)
		}
	}
	for _, tf := range f.Vocabularies["timeInForce"] {
		if !contracts.TimeInForce(tf).Valid() {
			t.Errorf("fixture time-in-force %q unknown to Go", tf)
		}
	}
	for _, s := range f.Vocabularies["tradingStatus"] {
		if !contracts.TradingStatus(s).Known() {
			t.Errorf("fixture trading status %q unknown to Go", s)
		}
	}
	for _, v := range f.Vocabularies["schemaVersion"] {
		if !contracts.SchemaVersion(v).SupportedSchemaVersion() {
			t.Errorf("fixture schema version %q unsupported by Go", v)
		}
	}
	if len(f.Vocabularies["errorCode"]) != len(contracts.CanonicalErrorCodes()) {
		t.Errorf("error code count: fixture %d, Go %d",
			len(f.Vocabularies["errorCode"]), len(contracts.CanonicalErrorCodes()))
	}
}

func TestSchemaFileDeclaresSameVocabulariesAsGo(t *testing.T) {
	t.Parallel()
	// The published JSON Schema and the Go implementation must declare the same
	// closed vocabularies. A mismatch means a consumer could read a value Go
	// would reject (or vice versa).
	raw, err := os.ReadFile(filepath.Join("schema", "common.schema.json"))
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	var doc struct {
		Defs map[string]struct {
			Enum []string `json:"enum"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse schema: %v", err)
	}
	check := func(defName string, goValues []string) {
		d, ok := doc.Defs[defName]
		if !ok {
			t.Fatalf("schema is missing the %q definition", defName)
		}
		if len(d.Enum) != len(goValues) {
			t.Errorf("schema %q has %d members, Go has %d", defName, len(d.Enum), len(goValues))
			return
		}
		set := map[string]bool{}
		for _, v := range goValues {
			set[v] = true
		}
		for _, v := range d.Enum {
			if !set[v] {
				t.Errorf("schema %q declares %q which Go does not accept", defName, v)
			}
		}
	}
	codes := make([]string, 0)
	for _, c := range contracts.CanonicalErrorCodes() {
		codes = append(codes, string(c))
	}
	check("errorCode", codes)
	envs := make([]string, 0)
	for _, e := range []contracts.Environment{
		contracts.EnvDev, contracts.EnvTest, contracts.EnvStaging,
		contracts.EnvPaper, contracts.EnvShadow, contracts.EnvLive,
	} {
		envs = append(envs, string(e))
	}
	check("environment", envs)
	mkts := make([]string, 0)
	for _, m := range contracts.MarketClasses() {
		mkts = append(mkts, string(m))
	}
	check("marketClass", mkts)
	schemaVersions := make([]string, 0)
	for _, v := range contracts.CanonicalSchemaVersions() {
		schemaVersions = append(schemaVersions, string(v))
	}
	check("schemaVersion", schemaVersions)

	// entityType was previously unchecked, which is how a value with no
	// counterpart in this package ("cqt") survived in a published contract for
	// as long as the file existed. A consumer generating code from the schema
	// would have minted identifiers that ParseID rejects.
	entities := make([]string, 0)
	for _, e := range contracts.EntityTypes() {
		entities = append(entities, string(e))
	}
	check("entityType", entities)
}

func TestCanonicalTimestampFormatMatchesLayout(t *testing.T) {
	t.Parallel()
	// Guards the literal layout string against accidental edit.
	ts := contracts.TimestampAt(0)
	want := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC).Format(contracts.TimestampLayout)
	if ts.String() != want {
		t.Fatalf("layout drift: got %q, want %q", ts.String(), want)
	}
}
