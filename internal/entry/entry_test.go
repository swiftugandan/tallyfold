package entry

import (
	"errors"
	"testing"
)

func TestParseRoundTrip(t *testing.T) {
	lines := []string{
		"- Prefers short bullet summaries",
		"- Release builds need BuildKit 0.17 pinned [source: https://example.com/s/1; added: 2026-10-07]",
		"- Priya owns pricing; see [[team_structure]]",
		"- Uses [[metrics/keep_rate.sql]] for the keep rate [added: 2026-10-07; by: claude-code]",
	}
	for _, line := range lines {
		e, err := Parse(line)
		if err != nil {
			t.Fatalf("Parse(%q): %v", line, err)
		}
		if got := e.String(); got != line {
			t.Errorf("round trip changed the line:\n got %q\nwant %q", got, line)
		}
	}
}

func TestParseSplitsTextFromMetadata(t *testing.T) {
	e, err := Parse("- Deadline is 2026-10-15 [source: s/102; added: 2026-09-03]")
	if err != nil {
		t.Fatal(err)
	}
	if e.Text != "Deadline is 2026-10-15" || e.Get("source") != "s/102" || e.Get("added") != "2026-09-03" {
		t.Fatalf("unexpected parse: %+v", e)
	}
}

func TestParseRejectsMalformedMetadata(t *testing.T) {
	for _, line := range []string{
		"- Fact [source: a; source: b]",
		"- Fact [source: ]",
	} {
		if _, err := Parse(line); err == nil {
			t.Errorf("Parse(%q) accepted malformed metadata", line)
		}
	}
}

func TestValidate(t *testing.T) {
	cases := map[string]Entry{
		"empty":         {Text: "  "},
		"multi-line":    {Text: "one\ntwo"},
		"bad key":       {Text: "x", Meta: []Field{{"Source", "y"}}},
		"bad value":     {Text: "x", Meta: []Field{{"source", "a;b"}}},
		"ambiguous end": {Text: "Pinned version [note: keep]"},
	}
	for name, e := range cases {
		if err := e.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Validate() = %v, want ErrInvalid", name, err)
		}
	}
	ok := Entry{Text: "Prefers tables for comparisons", Meta: []Field{{"added", "2026-10-07"}}}
	if err := ok.Validate(); err != nil {
		t.Errorf("valid entry rejected: %v", err)
	}
}

func TestSetKeepsPosition(t *testing.T) {
	e := Entry{Text: "x", Meta: []Field{{"source", "a"}, {"added", "2026-01-01"}}}
	e.Set("source", "b")
	e.Set("by", "cli")
	if got, want := e.String(), "- x [source: b; added: 2026-01-01; by: cli]"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
