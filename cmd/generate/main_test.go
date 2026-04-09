package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"text/template"
)

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
	// Find the repo root by walking up from cwd to find go.mod,
	// same logic as findRootDir but without os.Exit.
	rootDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(rootDir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(rootDir)
		if parent == rootDir {
			t.Skip("cannot find go.mod — skipping deterministic output test")
		}
		rootDir = parent
	}

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
