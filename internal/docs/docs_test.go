package docs

import (
	"strings"
	"testing"
)

func TestDoc_AllKinds(t *testing.T) {
	for _, k := range Kinds() {
		got, err := Doc(Kind(k))
		if err != nil {
			t.Errorf("Doc(%q) error: %v", k, err)
			continue
		}
		if strings.TrimSpace(got) == "" {
			t.Errorf("Doc(%q) returned empty content", k)
		}
	}
}

func TestDoc_UnknownKind(t *testing.T) {
	if _, err := Doc(Kind("nope")); err == nil {
		t.Error("Doc(unknown) should return an error")
	}
}

func TestKinds(t *testing.T) {
	got := Kinds()
	want := []string{"index", "full", "api_openapi", "relay_openapi"}
	if len(got) != len(want) {
		t.Fatalf("Kinds() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Kinds()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestIsMarkdown(t *testing.T) {
	for _, tt := range []struct {
		kind Kind
		want bool
	}{
		{KindIndex, true},
		{KindFull, true},
		{KindAPIOpenAPI, false},
		{KindRelayOpenAPI, false},
	} {
		if got := IsMarkdown(tt.kind); got != tt.want {
			t.Errorf("IsMarkdown(%q) = %v, want %v", tt.kind, got, tt.want)
		}
	}
}

func TestBestPractices_HasTopicHeadings(t *testing.T) {
	bp, err := BestPractices()
	if err != nil {
		t.Fatalf("BestPractices() error: %v", err)
	}
	// The get_pushward_best_practices topic enum maps directly to these H2s;
	// if a heading is renamed without updating the enum, topic slicing breaks.
	for _, topic := range []string{"integration", "live-activity", "relay-provider"} {
		if !strings.Contains(bp, "## "+topic) {
			t.Errorf("best-practices.md missing heading %q", "## "+topic)
		}
		text, ok, _ := Section(bp, topic)
		if !ok {
			t.Errorf("Section(best-practices, %q) did not match", topic)
			continue
		}
		if !strings.HasPrefix(strings.TrimSpace(text), "## "+topic) {
			t.Errorf("Section(%q) should start at its heading, got: %.40q", topic, text)
		}
	}
}

const fenceDoc = "# Title\n\nintro\n\n" +
	"## Real Section\n\n```sh\n# not a heading\necho hi\n```\n\nbody text\n\n" +
	"## Next Section\n\ntail\n"

func TestSection_ExactAndSubstring(t *testing.T) {
	// Slug match.
	text, ok, _ := Section(fenceDoc, "real-section")
	if !ok {
		t.Fatal("expected slug match for real-section")
	}
	if !strings.Contains(text, "## Real Section") || strings.Contains(text, "## Next Section") {
		t.Errorf("slice did not stop at the next same-level heading:\n%s", text)
	}
	// Substring (case-insensitive) match.
	text2, ok2, _ := Section(fenceDoc, "REAL")
	if !ok2 || text2 != text {
		t.Errorf("substring match diverged from slug match")
	}
}

func TestSection_IgnoresFencedHashes(t *testing.T) {
	// The '#' line lives inside a code fence — it must be content, never a
	// heading boundary, and must not be matchable as a section.
	text, ok, _ := Section(fenceDoc, "real-section")
	if !ok {
		t.Fatal("expected match")
	}
	if !strings.Contains(text, "# not a heading") {
		t.Errorf("fenced code content was dropped:\n%s", text)
	}
	if _, matched, _ := Section(fenceDoc, "not a heading"); matched {
		t.Error("a '#' line inside a code fence must not be matchable as a heading")
	}
}

func TestSection_CRLF(t *testing.T) {
	crlf := strings.ReplaceAll(fenceDoc, "\n", "\r\n")
	text, ok, _ := Section(crlf, "Real Section")
	if !ok {
		t.Fatal("expected match on CRLF input")
	}
	if strings.Contains(text, "\r") {
		t.Errorf("CRLF not normalized in output: %q", text)
	}
	if !strings.Contains(text, "## Real Section") {
		t.Errorf("CRLF slice missing heading: %q", text)
	}
}

func TestSection_Empty(t *testing.T) {
	text, ok, _ := Section(fenceDoc, "   ")
	if !ok {
		t.Fatal("blank section should return the whole doc")
	}
	if text != fenceDoc {
		t.Errorf("blank section should return the unmodified (normalized) doc")
	}
}

func TestSection_NoMatch(t *testing.T) {
	text, ok, topLevel := Section(fenceDoc, "does-not-exist")
	if ok {
		t.Errorf("expected no match, got: %s", text)
	}
	want := []string{"Real Section", "Next Section"}
	if len(topLevel) != len(want) {
		t.Fatalf("topLevel = %v, want %v", topLevel, want)
	}
	for i := range want {
		if topLevel[i] != want[i] {
			t.Errorf("topLevel[%d] = %q, want %q", i, topLevel[i], want[i])
		}
	}
}

func TestSection_RealFullBundle(t *testing.T) {
	full, err := Doc(KindFull)
	if err != nil {
		t.Fatal(err)
	}
	text, ok, _ := Section(full, "Countdown Template")
	if !ok {
		t.Fatal("expected to find 'Countdown Template' in llms-full.txt")
	}
	if !strings.Contains(text, "Countdown") {
		t.Error("Countdown section missing its own content")
	}
	if strings.Contains(text, "# Steps Template") || strings.Contains(text, "# Generic Template") {
		t.Errorf("Countdown slice leaked into adjacent H1 sections:\n%.200s", text)
	}
}

func TestNavigableHeadings_PrefersShallowestMultiLevel(t *testing.T) {
	// Single H1 with several H2s (the llms.txt shape) → H2s are the nav set.
	indexShape := parseHeadings("# Root\n\n## A\n\ntext\n\n## B\n\ntext\n\n## C\n")
	got := navigableHeadings(indexShape)
	if len(got) != 3 || got[0] != "A" || got[2] != "C" {
		t.Errorf("navigableHeadings(index shape) = %v, want [A B C]", got)
	}
	// Many H1s (the llms-full.txt shape) → H1s are the nav set.
	fullShape := parseHeadings("# One\n\n## sub\n\n# Two\n\n# Three\n")
	got2 := navigableHeadings(fullShape)
	if len(got2) != 3 || got2[0] != "One" {
		t.Errorf("navigableHeadings(full shape) = %v, want [One Two Three]", got2)
	}
}

func TestSlugify(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"Countdown Template", "countdown-template"},
		{"live-activity", "live-activity"},
		{"live_activity", "live-activity"},
		{`1\. Install the iOS App`, "1-install-the-ios-app"},
		{"  Examples & Limits  ", "examples-limits"},
		{"API Reference", "api-reference"},
	} {
		if got := slugify(tt.in); got != tt.want {
			t.Errorf("slugify(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
