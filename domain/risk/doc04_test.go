package risk

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// doc04Path is doc 04, which is the authority for the risk gate. Doc 04 is the
// third document in the precedence order (25 > 23 > 03 > 04 > 21 > 22 > 24), so
// where it and the schema disagree the schema may only add a stricter control --
// but it cannot invent a check the specification does not name, and it cannot
// drop one it does.
const doc04Path = "../../docs/04_TRADING_DOMAIN_AND_RISK.md"

// controlSource names the bullet in doc 04's risk gate list that a control
// implements.
//
// The left column is a phrase that must appear verbatim in the document. That is
// the whole point of this file: the association between a Go identifier and a
// specification bullet cannot itself be derived from the specification, so
// somebody has to assert it -- and if the assertion lives only in a comment, it
// is a claim nobody re-checks. Here it is a table, and the right-hand column is
// tied to the real document, so a reworded bullet fails the build instead of
// quietly invalidating the mapping.
//
// The phrases are listed in doc 04's order, which is the order Controls() uses.
var controlSource = []struct {
	phrase   string
	control  Control
	consumes bool // true when the control compares a policy column
}{
	{"account and environment authorization", CtrlAccountAuthorized, false},
	{"strategy deployment status", CtrlStrategyDeployed, false},
	{"instrument eligibility", CtrlInstrumentEligible, false},
	{"venue availability", CtrlVenueAvailable, false},
	{"price and quantity precision", CtrlPricePrecision, false},
	{"notional limits", CtrlNotionalLimit, true},
	{"position limits", CtrlPositionLimit, true},
	{"exposure limits", CtrlExposureLimit, true},
	{"concentration limits", CtrlConcentrationLimit, true},
	{"leverage limits", CtrlLeverageLimit, true},
	{"loss and drawdown controls", CtrlLossLimit, true},
	{"stale market-data controls", CtrlMarketDataFreshness, false},
	{"duplicate-order detection", CtrlDuplicateOrder, false},
	{"rate and throttle limits", CtrlRateLimit, true},
	{"kill/halt state", CtrlHaltState, false},
}

// readDoc04 returns the text of doc 04.
//
// A missing or unreadable document is a failure, never a skip. A skip would
// leave the gate unpinned in exactly the environment where it matters -- a
// checkout that lacks the specification -- and a skipped test reads as a passed
// one in every summary it appears in.
func readDoc04(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(doc04Path))
	if err != nil {
		t.Fatalf("doc 04 is the authority for the risk gate and could not be read: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("doc 04 is empty")
	}
	return string(b)
}

// gateBullets extracts the bullet list under doc 04's "## Risk gate" heading,
// which is the enumeration of mandatory checks.
//
// Extraction stops at the next heading rather than at a fixed line number, so
// inserting a paragraph above the list does not require editing this test.
func gateBullets(t *testing.T, doc string) []string {
	t.Helper()
	lines := strings.Split(doc, "\n")

	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == "## Risk gate" {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("doc 04 has no '## Risk gate' section, so the control list cannot be pinned to it")
	}

	var bullets []string
	for _, raw := range lines[start+1:] {
		l := strings.TrimSpace(raw)
		if strings.HasPrefix(l, "## ") {
			break
		}
		if l == "" {
			continue
		}
		if strings.HasPrefix(l, "- ") {
			bullets = append(bullets, strings.TrimPrefix(l, "- "))
			continue
		}
		// Prose before the list is the lead-in ("...deterministic checks for:")
		// and prose after it ends the section ("A single failed mandatory
		// control rejects the order."). Markdown terminates a list that way, so
		// the first non-bullet line after the list has begun closes it.
		if len(bullets) > 0 {
			break
		}
	}
	if len(bullets) == 0 {
		t.Fatal("doc 04's risk gate section names no mandatory checks, so the control " +
			"enumeration cannot be pinned to it")
	}
	return bullets
}

// The control set is doc 04's list, in doc 04's order.
//
// This is the direction nothing else covers. TestColumnsWithNoDoc04Control pins
// the policy columns to the controls; TestEveryConsultedColumnResolves pins the
// consulted columns to their bounds; the parity test pins the projection to
// information_schema. All three are consistent with a control having been
// dropped from Controls() entirely, because each of them iterates whatever
// Controls() happens to return. The enumeration's completeness was therefore
// only ever a comment.
func TestTheControlsAreDoc04sListInDoc04sOrder(t *testing.T) {
	doc := readDoc04(t)
	bullets := gateBullets(t, doc)

	controls := Controls()

	if len(bullets) != len(controls) {
		t.Fatalf("doc 04 names %d mandatory checks but the gate implements %d.\n"+
			"doc 04: %v\ngate:   %v\n"+
			"A control with no bullet is a check the specification does not require; "+
			"a bullet with no control is a check the specification requires and the gate does not perform.",
			len(bullets), len(controls), bullets, controls)
	}
	if len(controlSource) != len(bullets) {
		t.Fatalf("controlSource maps %d bullets, but doc 04 names %d", len(controlSource), len(bullets))
	}

	for i, b := range bullets {
		want := controlSource[i]

		if b != want.phrase {
			t.Errorf("doc 04 bullet %d is %q, but this test maps that position to %q.\n"+
				"Either doc 04's list was reordered or reworded, or the mapping here is stale. "+
				"Re-read the bullet list and correct this table -- do not reorder the gate to match a guess.",
				i+1, b, want.phrase)
		}
		if !strings.Contains(doc, want.phrase) {
			t.Errorf("controlSource names the phrase %q for %s, which does not appear in doc 04",
				want.phrase, want.control)
		}

		got := controls[i]
		if got != want.control {
			t.Errorf("bullet %d (%q) is implemented by %s, but the gate runs it in position %d as %s.\n"+
				"The order is load-bearing: a finding names its control, and two readers comparing doc 04 "+
				"to a report need the lists to line up.", i+1, b, want.control, i+1, got)
		}
	}
}

// Every control appears exactly once, and the mapping covers every control. A
// control named twice in the table would silently satisfy both the count above
// and the ordering check while one control went unrun.
func TestNoControlIsMappedTwiceOrLeftUnmapped(t *testing.T) {
	seen := make(map[Control]string, len(controlSource))
	for _, m := range controlSource {
		if prev, dup := seen[m.control]; dup {
			t.Errorf("%s is mapped to both %q and %q", m.control, prev, m.phrase)
			continue
		}
		seen[m.control] = m.phrase
	}

	for _, c := range Controls() {
		if _, ok := seen[c]; !ok {
			t.Errorf("%s is in the gate but in no row of controlSource, so nothing ties it to doc 04", c)
		}
	}
	if !CtrlHaltState.Known() {
		t.Error("kill/halt state is not a known control, though doc 04 requires it and it is unwaivable")
	}
}

// The consumes flag is a claim about which controls take a number, and
// RequiresLimit makes the same claim in code. Two claims about the same thing
// drift apart; this is the test that notices.
//
// It is worth pinning because the seven numeric controls are exactly the ones
// that deny when no bound is configured. If RequiresLimit ever admitted a control
// that consults no column, that control would start denying for want of a limit
// it never needed -- every order refused for a reason with no number behind it.
func TestTheControlsThatTakeANumberAreTheOnesThatSaySo(t *testing.T) {
	for _, m := range controlSource {
		if m.control.RequiresLimit() != m.consumes {
			t.Errorf("%s (doc 04: %q) RequiresLimit()=%v but controlSource says it consumes a limit: %v.\n"+
				"A control that takes a number must deny when the bound is absent, and a control that takes "+
				"no number must not invent one.", m.control, m.phrase, m.control.RequiresLimit(), m.consumes)
		}
	}
}

// The halt hierarchy is the second closed enumeration doc 04 defines, and it is
// pinned the same way in domain/halt/doc04_test.go, where the levels live. It is
// not repeated here because importing domain/halt from this package would couple
// the gate's pure logic to the halt store for no reason.
