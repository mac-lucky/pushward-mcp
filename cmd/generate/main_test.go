package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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

// apiSpec parses the committed root API spec, skipping rather than failing where
// the file is absent (a stripped sandbox), which is what every spec-backed test
// wants.
func apiSpec(t *testing.T) *openAPISpec {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "openapi.yaml"))
	if err != nil {
		t.Skipf("openapi.yaml not found: %v", err)
	}
	return parseSpecJSON(data, "api")
}

// clauseValues pulls the pipe-delimited list out of one clause of a hand-written
// description - the templates between "Images (" and " only", the shapes between
// "image_shape (" and ", default square)". Matching the delimited clause rather
// than the whole string is the point: half these names are ordinary words in the
// surrounding prose, so a plain Contains passes on a description that never lists
// them. Both delimiters are literal and the closing one is searched after the
// opening, so a reworded description fails as a wording problem here rather than
// as a phantom spec mismatch further down.
func clauseValues(t *testing.T, desc, open, closing string) []string {
	t.Helper()
	i := strings.Index(desc, open)
	if i < 0 {
		t.Fatalf("description carries no %q clause; that delimiter is literal and has to move with the wording: %s", open, desc)
	}
	rest := desc[i+len(open):]
	j := strings.Index(rest, closing)
	if j < 0 {
		t.Fatalf("the %q clause is not closed by the literal %q: %s", open, closing, rest)
	}
	values := strings.Split(rest[:j], "|")
	slices.Sort(values)
	return values
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
	tools := buildAPITools(apiSpec(t))
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
	// must mention the widget template set. countdown and gauge are no longer a
	// tell - both schemas carry them - so test the ones only activities have.
	for _, activityOnly := range []string{"generic", "steps", "timeline"} {
		if d := byName["create_widget"].ContentDesc; strings.Contains(d, activityOnly) {
			t.Errorf("create_widget content desc leaks activity template %q: %s", activityOnly, d)
		}
	}
	if d := byName["create_widget"].ContentDesc; !strings.Contains(d, "stat_list") {
		t.Errorf("create_widget content desc missing widget templates: %s", d)
	}
}

// The widget content description is hand-written while the enum it documents
// lives in the spec, so the two drift silently: the 1.6 templates sat in the
// spec for a release while the description still advertised five. Every enum
// value must appear in the description a coding agent reads.
func TestWidgetTemplateEnumParity(t *testing.T) {
	spec := apiSpec(t)
	widget, ok := spec.Components.Schemas[widgetContentSchema]
	if !ok {
		t.Fatalf("%s schema missing from openapi.yaml", widgetContentSchema)
	}
	tmpl, ok := widget.Properties["template"]
	if !ok || len(tmpl.Enum) == 0 {
		t.Fatalf("%s.template carries no enum", widgetContentSchema)
	}
	listed := clauseValues(t, contentJSONDesc(true, "POST"), "template (", " - selects the visual style)")
	for _, name := range tmpl.Enum {
		if !slices.Contains(listed, name) {
			t.Errorf("widget template %q is in the spec enum but absent from the content_json description", name)
		}
	}
	for _, name := range listed {
		if !slices.Contains(tmpl.Enum, name) {
			t.Errorf("widget template %q is advertised but the spec enum does not carry it", name)
		}
	}
}

// The image trio is accepted on the generic and steps templates only - the
// server 422s it anywhere else - so a description that names the fields without
// the restriction sends agents into a rejected request. Field names, template
// list, shape enum and length caps are all hand-written against a spec that
// moves, the same drift the widget enum hit.
func TestActivityImageFieldParity(t *testing.T) {
	spec := apiSpec(t)
	activity, ok := spec.Components.Schemas["Activity"]
	if !ok {
		t.Fatal("Activity schema missing from openapi.yaml")
	}
	content, ok := activity.Properties["content"]
	if !ok || content.Discriminator == nil || len(content.Discriminator.Mapping) == 0 {
		t.Fatal("Activity.content carries no discriminator mapping")
	}
	// The mapping is only a statement about templates while it is keyed on the
	// template field; repointed at anything else it still parses, and everything
	// below would then be walking the wrong axis.
	if got := content.Discriminator.PropertyName; got != "template" {
		t.Fatalf("Activity.content discriminates on %q, not template - the image restriction is stated per template", got)
	}

	// Which templates accept an image, which fields they take, and the bounds on
	// those fields, all read off the spec rather than restated here - restating is
	// what drifts.
	fields := make(map[string]bool)
	maxLengths := make(map[string]int)
	var shapes []string
	var templates []string
	for template, ref := range content.Discriminator.Mapping {
		schema, ok := spec.Components.Schemas[refTypeName(ref)]
		if !ok {
			t.Errorf("template %q maps to %q, which is not a component schema", template, ref)
			continue
		}
		carries := false
		for name, prop := range schema.Properties {
			if !strings.HasPrefix(name, "image_") {
				continue
			}
			fields[name] = true
			carries = true
			if prop.MaxLength != nil {
				if prior, seen := maxLengths[name]; seen && prior != *prop.MaxLength {
					t.Errorf("%s caps at %d on template %q but %d on another - the description can only state one", name, *prop.MaxLength, template, prior)
				}
				maxLengths[name] = *prop.MaxLength
			}
			if name == "image_shape" && len(prop.Enum) > 0 {
				sorted := slices.Clone(prop.Enum)
				slices.Sort(sorted)
				if shapes != nil && !slices.Equal(shapes, sorted) {
					t.Errorf("image_shape offers %v on template %q but %v on another", sorted, template, shapes)
				}
				shapes = sorted
			}
		}
		if carries {
			templates = append(templates, template)
		}
	}
	if len(fields) == 0 {
		t.Fatal("no image_ fields on any activity content schema - the committed openapi.yaml is behind the server")
	}
	slices.Sort(templates)

	// PATCH is the variant that ships: CreateActivityRequest has no content
	// property, so content_json reaches an agent only on update_activity.
	desc := contentJSONDesc(false, "PATCH")
	for name := range fields {
		if !strings.Contains(desc, name) {
			t.Errorf("activity content field %q is in the spec but absent from the content_json description", name)
		}
	}
	for _, name := range regexp.MustCompile(`image_[a-z_]+`).FindAllString(desc, -1) {
		if !fields[name] {
			t.Errorf("the content_json description advertises %q, which no activity content schema carries", name)
		}
	}

	if listed := clauseValues(t, desc, "Images (", " only"); !slices.Equal(listed, templates) {
		t.Errorf("the image clause lists templates %v, the spec carries image fields on %v", listed, templates)
	}
	if len(shapes) == 0 {
		t.Error("image_shape carries no enum in the spec, so the shapes the description offers are unpinned")
	} else if listed := clauseValues(t, desc, "image_shape (", ", default square)"); !slices.Equal(listed, shapes) {
		t.Errorf("the description offers shapes %v, the spec enum is %v", listed, shapes)
	}

	// A cap the description overstates is a request the agent builds and the
	// server rejects, so both numbers come from the spec too.
	maxRe := regexp.MustCompile(`max (\d+)`)
	for _, field := range []string{"image_url", "image_thumbhash"} {
		want, ok := maxLengths[field]
		if !ok {
			t.Errorf("%s carries no maxLength in the spec, so the cap the description states is unpinned", field)
			continue
		}
		i := strings.Index(desc, field+" (")
		if i < 0 {
			t.Errorf("the description does not open a %q clause", field+" (")
			continue
		}
		clause := desc[i:]
		if end := strings.Index(clause, ")"); end >= 0 {
			clause = clause[:end]
		}
		m := maxRe.FindStringSubmatch(clause)
		if m == nil {
			t.Errorf("the %s clause states no \"max <n>\": %s", field, clause)
			continue
		}
		if m[1] != formatBound(float64(want)) {
			t.Errorf("the %s clause caps at %s, the spec maxLength is %d", field, m[1], want)
		}
	}

	// Widgets have no image support at all. If the server grows it, the widget
	// description needs its own clause before this assertion is relaxed.
	widget, ok := spec.Components.Schemas[widgetContentSchema]
	if !ok {
		t.Fatalf("%s schema missing from openapi.yaml", widgetContentSchema)
	}
	for name := range widget.Properties {
		if strings.HasPrefix(name, "image_") {
			t.Errorf("%s gained %q but the widget content_json description documents no images", widgetContentSchema, name)
		}
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
	for _, activityOnly := range []string{"generic", "steps", "timeline"} {
		if strings.Contains(wPost, activityOnly) {
			t.Errorf("widget desc should not mention activity template %q: %s", activityOnly, wPost)
		}
	}
	// The 1.6 additions rode the spec for a release before this string caught up.
	for _, added := range []string{"trend", "countdown", "battery", "schedule", "flow"} {
		if !strings.Contains(wPost, added) {
			t.Errorf("widget desc missing template %q: %s", added, wPost)
		}
	}
	// Widgets have no image support - advertising the activity trio there is a
	// guaranteed rejected request.
	if strings.Contains(wPost, "image_") {
		t.Errorf("widget desc should not mention activity image fields: %s", wPost)
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
