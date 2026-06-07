package tools

import (
	"strings"
	"testing"
)

func TestHandleGetPushwardDocs_AllKinds(t *testing.T) {
	for _, kind := range []string{"index", "full", "api_openapi", "relay_openapi"} {
		result, err := handleGetPushwardDocs(newReq(map[string]any{"kind": kind}))
		if err != nil {
			t.Fatalf("kind=%s: unexpected error: %v", kind, err)
		}
		if result.IsError {
			t.Errorf("kind=%s: IsError, got: %s", kind, resultText(t, result))
			continue
		}
		if strings.TrimSpace(resultText(t, result)) == "" {
			t.Errorf("kind=%s: empty result", kind)
		}
	}
}

func TestHandleGetPushwardDocs_InvalidKind(t *testing.T) {
	result, err := handleGetPushwardDocs(newReq(map[string]any{"kind": "bogus"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected IsError for invalid kind")
	}
	text := resultText(t, result)
	if !strings.Contains(text, "invalid kind") || !strings.Contains(text, "index") {
		t.Errorf("error should list valid kinds, got: %s", text)
	}
}

func TestHandleGetPushwardDocs_MissingKind(t *testing.T) {
	result, err := handleGetPushwardDocs(newReq(map[string]any{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected IsError when kind is missing")
	}
}

func TestHandleGetPushwardDocs_FullWithSection(t *testing.T) {
	result, err := handleGetPushwardDocs(newReq(map[string]any{
		"kind":    "full",
		"section": "Countdown Template",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := resultText(t, result)
	if result.IsError {
		t.Fatalf("unexpected error result: %s", text)
	}
	if !strings.Contains(text, "Countdown") {
		t.Errorf("expected Countdown content, got: %.120s", text)
	}
	// Slicing must shrink the result well below the full bundle.
	full, _ := handleGetPushwardDocs(newReq(map[string]any{"kind": "full"}))
	if len(text) >= len(resultText(t, full)) {
		t.Error("section slice was not smaller than the full bundle")
	}
}

func TestHandleGetPushwardDocs_SectionIgnoredForOpenAPI(t *testing.T) {
	withSection, _ := handleGetPushwardDocs(newReq(map[string]any{
		"kind":    "api_openapi",
		"section": "Countdown Template",
	}))
	whole, _ := handleGetPushwardDocs(newReq(map[string]any{"kind": "api_openapi"}))
	if resultText(t, withSection) != resultText(t, whole) {
		t.Error("section must be ignored for api_openapi (full spec expected)")
	}
}

func TestHandleGetPushwardDocs_SectionNoMatch(t *testing.T) {
	result, err := handleGetPushwardDocs(newReq(map[string]any{
		"kind":    "full",
		"section": "this-section-does-not-exist",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Error("a no-match section should return guidance, not an error")
	}
	text := resultText(t, result)
	if !strings.Contains(text, "No section matching") || !strings.Contains(text, "Available headings") {
		t.Errorf("expected available-headings guidance, got: %.160s", text)
	}
}

func TestHandleGetPushwardBestPractices_Full(t *testing.T) {
	result, err := handleGetPushwardBestPractices(newReq(map[string]any{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}
	text := resultText(t, result)
	for _, topic := range bestPracticeTopics {
		if !strings.Contains(text, "## "+topic) {
			t.Errorf("full guide missing %q section", topic)
		}
	}
}

func TestHandleGetPushwardBestPractices_Topic(t *testing.T) {
	result, err := handleGetPushwardBestPractices(newReq(map[string]any{"topic": "relay-provider"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := resultText(t, result)
	if !strings.HasPrefix(strings.TrimSpace(text), "## relay-provider") {
		t.Errorf("expected slice to start at relay-provider heading, got: %.60s", text)
	}
	if strings.Contains(text, "## integration") {
		t.Errorf("relay-provider slice leaked into another topic:\n%.200s", text)
	}
}

func TestHandleGetPushwardBestPractices_UnknownTopic(t *testing.T) {
	result, err := handleGetPushwardBestPractices(newReq(map[string]any{"topic": "nonsense"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Error("unknown topic should return guidance, not an error")
	}
	if !strings.Contains(resultText(t, result), "No section matching") {
		t.Errorf("expected guidance for unknown topic, got: %s", resultText(t, result))
	}
}
