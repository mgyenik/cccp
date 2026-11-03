package fuzzy

import "testing"

func TestMatchBasics(t *testing.T) {
	for _, c := range []struct {
		q, text string
		ok      bool
	}{
		{"", "anything", true},
		{"gain", "Gain staging", true},
		{"gs", "Gain staging", true},
		{"gstag", "Gain staging", true},       // g|stag: word starts
		{"vitrbi", "Viterbi algorithm", true}, // vit|rbi: runs of 3
		{"96", "AX96 amplifier", true},        // one run: a substring
		{"gnstg", "Gain staging", false},
		{"tebi", "Viterbi algorithm", false},                                  // te|bi: two-letter runs inside a word
		{"gsn", "Gain staging", false},                                        // g|s|n: the last run is the lone letter                                      // g|n|st|g: "n" is a lone letter mid-word
		{"viterbi", "an avid kitten, very bitter", false}, // vi|t|e|r|bi
		{"stag gain", "Gain staging", true},                                   // terms in any order
		{"xyz", "Gain staging", false},
		{"gain xyz", "Gain staging", false}, // every term must match
		{"ax96", "AX96 amplifier spice simulation", true},
		{"é", "café", true},
	} {
		if _, ok := Match(c.q, c.text); ok != c.ok {
			t.Errorf("Match(%q, %q) ok=%v want %v", c.q, c.text, ok, c.ok)
		}
	}
}

func TestRankingPrefersPrefixWordStartAndContiguous(t *testing.T) {
	better := func(q, a, b string) {
		t.Helper()
		sa, oka := Match(q, a)
		sb, okb := Match(q, b)
		if !oka || !okb || sa <= sb {
			t.Errorf("%q: want %q (%d) above %q (%d)", q, a, sa, b, sb)
		}
	}
	better("gain", "gain staging", "regaining")            // prefix beats interior
	better("ota", "Firmware OTA image", "rotation")        // word start beats interior
	better("motor", "motor driver", "m-o-t-o-r scattered") // contiguous beats scattered
	better("qa", "acme qa tools", "q___________a")         // short gap beats long gap
}

func TestBestOccurrenceIsChosen(t *testing.T) {
	// The first 's' is mid-word; the match at the word start must win.
	s1, _ := Match("stag", "xs gain staging")
	s2, _ := Match("stag", "staging")
	if s1 < s2-scoreMatch { // both should find the word-start "stag"
		t.Fatalf("scored the poor occurrence: %d vs %d", s1, s2)
	}
}

func TestFieldsKeepTermsInsideOneField(t *testing.T) {
	// "gain" scattered across two fields is no match: g|ain.
	if _, ok := MatchFields("gain", Field{Text: "g"}, Field{Text: "ain"}); ok {
		t.Fatal("a term matched across fields")
	}
	// Different terms may match different fields.
	if _, ok := MatchFields("gain infra", Field{Text: "Gain staging"}, Field{Text: "/w/infra"}); !ok {
		t.Fatal("terms in different fields")
	}
}

func TestProseNeedsASubstring(t *testing.T) {
	prose := Field{Text: "Please validate the input, then add tests for every branch.", Prose: true}
	if _, ok := MatchFields("vtrb", prose); ok {
		t.Fatal("a scattered subsequence matched prose")
	}
	if _, ok := MatchFields("tests", prose); !ok {
		t.Fatal("a word in prose did not match")
	}
	if _, ok := MatchFields("VALID", prose); !ok {
		t.Fatal("prose matching is case-insensitive")
	}
	// The same term scores higher in a short field than in prose.
	short, _ := MatchFields("tests", Field{Text: "tests"})
	long, _ := MatchFields("tests", Field{Text: "tests", Prose: true})
	if long >= short {
		t.Fatalf("prose %d, short %d", long, short)
	}
	// The best occurrence in prose wins: the word start, not the interior.
	a, _ := MatchFields("stag", Field{Text: "xstag then staging", Prose: true})
	b, _ := MatchFields("stag", Field{Text: "xstag", Prose: true})
	if a <= b {
		t.Fatalf("best occurrence not chosen: %d vs %d", a, b)
	}
}
