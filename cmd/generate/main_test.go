package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
)

// repoRoot walks up from the test's cwd to the module root (where go.mod lives),
// skipping the test if it cannot be found (e.g. in a stripped sandbox).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("cannot find go.mod")
		}
		dir = parent
	}
}

func TestToSnakeCase(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"listActivities", "list_activities"},
		{"createActivity", "create_activity"},
		{"getMe", "get_me"},
		{"getHealth", "get_health"},
		{"postGrafanaWebhook", "post_grafana_webhook"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := toSnakeCase(tt.input)
			if got != tt.want {
				t.Errorf("toSnakeCase(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestToPascalCase(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"ended_ttl", "EndedTTL"},
		{"thread_id", "ThreadID"},
		{"image_url", "ImageURL"},
		{"collapse_id", "CollapseID"},
		{"source_display_name", "SourceDisplayName"},
		{"argocd", "Argocd"},
		{"api", "API"},
		{"stale_ttl", "StaleTTL"},
		{"dismissal_ttl", "DismissalTTL"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := toPascalCase(tt.input)
			if got != tt.want {
				t.Errorf("toPascalCase(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestSchemaType(t *testing.T) {
	tests := []struct {
		name string
		s    schemaObj
		want string
	}{
		{
			name: "string type",
			s:    schemaObj{Type: "string"},
			want: "string",
		},
		{
			name: "integer type",
			s:    schemaObj{Type: "integer"},
			want: "integer",
		},
		{
			name: "nullable integer",
			s:    schemaObj{Type: []any{"integer", "null"}},
			want: "integer",
		},
		{
			name: "nullable string",
			s:    schemaObj{Type: []any{"string", "null"}},
			want: "string",
		},
		{
			name: "int64 format no type",
			s:    schemaObj{Format: "int64"},
			want: "number",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := schemaType(tt.s)
			if got != tt.want {
				t.Errorf("schemaType() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsFlat(t *testing.T) {
	spec := &openAPISpec{} // empty spec, no components needed for these tests

	tests := []struct {
		name   string
		schema schemaObj
		want   bool
	}{
		{
			name: "4 flat string props",
			schema: schemaObj{
				Properties: map[string]schemaObj{
					"a": {Type: "string"},
					"b": {Type: "string"},
					"c": {Type: "string"},
					"d": {Type: "string"},
				},
			},
			want: true,
		},
		{
			name: "nested ref prop",
			schema: schemaObj{
				Properties: map[string]schemaObj{
					"a":      {Type: "string"},
					"nested": {Ref: "#/components/schemas/Foo"},
				},
			},
			want: false,
		},
		{
			name: "array of ref items",
			schema: schemaObj{
				Properties: map[string]schemaObj{
					"a":    {Type: "string"},
					"list": {Type: "array", Items: &schemaObj{Ref: "#/components/schemas/Bar"}},
				},
			},
			want: false,
		},
		{
			name: "12 flat props is ok",
			schema: func() schemaObj {
				props := make(map[string]schemaObj)
				for i := 0; i < 12; i++ {
					props[string(rune('a'+i))] = schemaObj{Type: "string"}
				}
				return schemaObj{Properties: props}
			}(),
			want: true,
		},
		{
			name: "13 flat props exceeds limit",
			schema: func() schemaObj {
				props := make(map[string]schemaObj)
				for i := 0; i < 13; i++ {
					props[string(rune('a'+i))] = schemaObj{Type: "string"}
				}
				return schemaObj{Properties: props}
			}(),
			want: false,
		},
		{
			name:   "empty schema",
			schema: schemaObj{},
			want:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isFlat(spec, tt.schema)
			if got != tt.want {
				t.Errorf("isFlat() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExtractPathParams(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		wantNames []string
	}{
		{
			name:      "single param",
			path:      "/activities/{slug}",
			wantNames: []string{"slug"},
		},
		{
			name:      "two params",
			path:      "/activities/{slug}/share/{codeID}",
			wantNames: []string{"slug", "codeID"},
		},
		{
			name:      "no params",
			path:      "/health",
			wantNames: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params := extractPathParams(tt.path)
			if len(params) != len(tt.wantNames) {
				t.Fatalf("extractPathParams(%q) returned %d params, want %d", tt.path, len(params), len(tt.wantNames))
			}
			for i, p := range params {
				if p.Name != tt.wantNames[i] {
					t.Errorf("param[%d].Name = %q, want %q", i, p.Name, tt.wantNames[i])
				}
				if !p.Required {
					t.Errorf("param[%d].Required = false, want true", i)
				}
				if p.MCPType != "String" {
					t.Errorf("param[%d].MCPType = %q, want %q", i, p.MCPType, "String")
				}
			}
		})
	}
}

func TestDeterministicOutput(t *testing.T) {
	rootDir := repoRoot(t)

	apiSpecPath := filepath.Join(rootDir, "openapi.yaml")
	relaySpecPath := filepath.Join(rootDir, "relay-openapi.json")

	// Verify spec files exist
	if _, err := os.Stat(apiSpecPath); err != nil {
		t.Skipf("openapi.yaml not found: %v", err)
	}
	if _, err := os.Stat(relaySpecPath); err != nil {
		t.Skipf("relay-openapi.json not found: %v", err)
	}

	generate := func() ([]byte, []byte) {
		apiData, _ := os.ReadFile(apiSpecPath)
		apiSpec := parseSpecJSON(apiData, "api")
		apiTools := buildAPITools(apiSpec)

		relayData, _ := os.ReadFile(relaySpecPath)
		relaySpec := parseSpecJSON(relayData, "relay")
		relayTools := buildRelayTools(relaySpec)

		apiOut := renderTemplate(apiToolsTemplate, apiTools)
		relayOut := renderTemplate(relayToolsTemplate, relayTools)
		return apiOut, relayOut
	}

	apiOut1, relayOut1 := generate()
	apiOut2, relayOut2 := generate()

	if string(apiOut1) != string(apiOut2) {
		t.Error("API tools output is not deterministic across two runs")
	}
	if string(relayOut1) != string(relayOut2) {
		t.Error("Relay tools output is not deterministic across two runs")
	}
}

func renderTemplate(tmpl *template.Template, data any) []byte {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func TestBuildAPITools_ExpectedSet(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "openapi.yaml"))
	if err != nil {
		t.Skipf("openapi.yaml not found: %v", err)
	}
	tools := buildAPITools(parseSpecJSON(data, "api"))
	if len(tools) == 0 {
		t.Fatal("buildAPITools produced zero tools")
	}
	byName := make(map[string]toolDef, len(tools))
	for _, tl := range tools {
		byName[tl.Name] = tl
	}
	for _, want := range []string{"create_activity", "create_notification", "update_activity", "create_widget", "update_widget"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("expected generated tool %q, missing", want)
		}
	}
	// listActivities and getActivity are handled by composite tools and must be
	// skipped, else they would collide with the hand-written handlers (compile
	// error). getActivity is superseded to add the include_log_backlog option.
	if _, ok := byName["list_activities"]; ok {
		t.Error("list_activities should be skipped (handled by composite tool)")
	}
	if _, ok := byName["get_activity"]; ok {
		t.Error("get_activity should be skipped (handled by composite tool with include_log_backlog)")
	}
	// create_activity has no content field; only the widget/activity updates do.
	if byName["create_activity"].ContentJSON {
		t.Error("create_activity should not use content_json (no content field)")
	}
	if !byName["create_widget"].ContentJSON {
		t.Error("create_widget should use content_json")
	}
	// The widget content description must not leak activity-only templates, and
	// must mention the widget template set.
	if d := byName["create_widget"].ContentDesc; strings.Contains(d, "countdown") {
		t.Errorf("create_widget content desc leaks activity templates: %s", d)
	}
	if d := byName["create_widget"].ContentDesc; !strings.Contains(d, "stat_list") {
		t.Errorf("create_widget content desc missing widget templates: %s", d)
	}
}

func TestBuildRelayTools_ExpectedSet(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "relay-openapi.json"))
	if err != nil {
		t.Skipf("relay-openapi.json not found: %v", err)
	}
	tools := buildRelayTools(parseSpecJSON(data, "relay"))
	got := make(map[string]bool, len(tools))
	for _, tl := range tools {
		got[tl.Name] = true
	}
	want := []string{
		"relay_argocd", "relay_backrest", "relay_bazarr", "relay_changedetection",
		"relay_forgejo", "relay_gatus", "relay_gitea", "relay_grafana",
		"relay_jellyfin", "relay_komodo", "relay_overseerr", "relay_paperless",
		"relay_prowlarr", "relay_proxmox", "relay_radarr", "relay_sonarr",
		"relay_unmanic", "relay_uptimekuma",
	}
	if len(tools) != len(want) {
		t.Errorf("got %d relay tools, want %d", len(tools), len(want))
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing relay tool %q", w)
		}
	}
}

func TestFormatBound(t *testing.T) {
	cases := map[float64]string{
		2592000: "2592000", // must not render as 2.592e+06
		3600:    "3600",
		1:       "1",
		0:       "0",
		0.5:     "0.5",
	}
	for in, want := range cases {
		if got := formatBound(in); got != want {
			t.Errorf("formatBound(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestContentJSONDesc(t *testing.T) {
	// Widget POST: full object, widget templates, no merge-patch wording.
	wPost := contentJSONDesc(true, "POST")
	if !strings.Contains(wPost, "stat_list") {
		t.Errorf("widget POST desc missing widget templates: %s", wPost)
	}
	if strings.Contains(wPost, "Merge Patch") {
		t.Errorf("widget POST desc should not mention merge-patch: %s", wPost)
	}
	if strings.Contains(wPost, "countdown") {
		t.Errorf("widget desc should not mention activity templates: %s", wPost)
	}
	// Activity-only board/log fields must not leak into the widget description.
	if strings.Contains(wPost, "tiles") || strings.Contains(wPost, "log_backlog") {
		t.Errorf("widget desc should not mention activity board/log fields: %s", wPost)
	}
	// Widget PATCH: merge-patch wording present.
	if wPatch := contentJSONDesc(true, "PATCH"); !strings.Contains(wPatch, "Merge Patch") {
		t.Errorf("widget PATCH desc should mention merge-patch: %s", wPatch)
	}
	// Activity PATCH: activity templates + merge-patch.
	aPatch := contentJSONDesc(false, "PATCH")
	if !strings.Contains(aPatch, "countdown") || !strings.Contains(aPatch, "Merge Patch") {
		t.Errorf("activity PATCH desc wrong: %s", aPatch)
	}
	// The board/log templates must be advertised in the enum and documented.
	if !strings.Contains(aPatch, "timeline|board|log)") {
		t.Errorf("activity desc missing board/log in template enum: %s", aPatch)
	}
	if !strings.Contains(aPatch, "board (tiles") || !strings.Contains(aPatch, "log (lines") {
		t.Errorf("activity desc missing board/log field docs: %s", aPatch)
	}
}
