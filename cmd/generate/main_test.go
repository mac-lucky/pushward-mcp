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
// TestWidgetContentFieldParity keeps the hand-written widget content_json prose
// in step with the spec. The description is prose, not text generated from the
// schema, so refreshing openapi.yaml on its own leaves a newly added server
// field undocumented and agents never learn it exists - which is exactly how
// device_sort shipped invisible. The activity side has the same guard in
// TestActivityImageFieldParity.
//
// Forward direction only. Pulling field names back out of prose would need a
// word-shaped regex that matches half the sentence, so an advertised-but-absent
// field is left to review; the drift that actually happens is the server growing
// a field this string never hears about.
func TestWidgetContentFieldParity(t *testing.T) {
	spec := apiSpec(t)
	widget, ok := spec.Components.Schemas[widgetContentSchema]
	if !ok {
		t.Fatalf("%s schema missing from openapi.yaml", widgetContentSchema)
	}
	if len(widget.Properties) == 0 {
		t.Fatalf("%s carries no properties - the committed openapi.yaml is behind the server", widgetContentSchema)
	}
	// POST and PATCH share the field list; only the lead sentence differs. The
	// template enum is stripped first: value, trend and flow are widget template
	// names as well as WidgetContent properties, so that clause alone would keep
	// the sweep green with the fields themselves undocumented.
	desc := strings.Replace(contentJSONDesc(true, "POST"), widgetTemplateEnumClause, "", 1)
	for name := range widget.Properties {
		if !mentionsField(desc, name) {
			t.Errorf("widget content field %q is in the spec but absent from the content_json description", name)
		}
	}
}

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
	for _, name := range tmpl.Enum {
		if !slices.Contains(widgetTemplateNames, name) {
			t.Errorf("widget template %q is in the spec enum but absent from the content_json description", name)
		}
	}
	for _, name := range widgetTemplateNames {
		if !slices.Contains(tmpl.Enum, name) {
			t.Errorf("widget template %q is advertised but the spec enum does not carry it", name)
		}
	}
}

// activityContentMapping returns the Activity.content discriminator mapping
// (template name -> content schema), failing the test when the spec no longer
// states it per template.
func activityContentMapping(t *testing.T, spec *openAPISpec) map[string]string {
	t.Helper()
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
	// reading it would then be walking the wrong axis.
	if got := content.Discriminator.PropertyName; got != "template" {
		t.Fatalf("Activity.content discriminates on %q, not template", got)
	}
	return content.Discriminator.Mapping
}

// The activity template list is prose, like the widget one, and it drifted the
// same way: board and log rode the spec before the description named them. The
// discriminator mapping is the spec's statement of which templates exist.
func TestActivityTemplateEnumParity(t *testing.T) {
	spec := apiSpec(t)
	mapping := activityContentMapping(t, spec)
	for name := range mapping {
		if !slices.Contains(activityTemplateNames, name) {
			t.Errorf("activity template %q is in the spec mapping but absent from the content_json description", name)
		}
	}
	for _, name := range activityTemplateNames {
		if _, ok := mapping[name]; !ok {
			t.Errorf("activity template %q is advertised but the spec mapping does not carry it", name)
		}
	}
}

// mentionsField reports whether desc names exactly this property. The widget
// side gets away with strings.Contains because no two widget field names nest;
// the activity set is full of pairs that do - url inside url_action, unit inside
// units, value inside min_value, state inside playback_state, progress inside
// live_progress - and a substring check passes on every one of them while the
// field itself goes undocumented. RE2's \b counts _ as a word character, which
// is the boundary these names need.
func mentionsField(desc, name string) bool {
	return regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`).MatchString(desc)
}

// The activity content description drifted for several releases while the spec
// grew step_rows, step_weights, step_colors, live_progress, primary_series and
// the content-level tap targets: the widget side has had a parity guard since
// device_sort shipped invisible, the activity side only had one for the media
// and image clauses. This is the general case.
//
// Forward direction only, like the widget test. Server-owned properties are
// exempted through the spec's own readOnly flag rather than a hand-list - the
// generator already skips read-only properties when it builds tool params, so
// the test agrees with it by construction - and the derived set is then pinned,
// so a field the server newly marks read-only fails here instead of silently
// falling out of the sweep. (The reverse - a field losing readOnly - is already
// loud: it enters the sweep and has to be documented.)
func TestActivityContentFieldParity(t *testing.T) {
	spec := apiSpec(t)
	mapping := activityContentMapping(t, spec)
	// The template enum is stripped for the same reason the widget sweep strips
	// its own: a field that shares a name with a template would match the enum
	// and never need documenting.
	desc := strings.Replace(contentJSONDesc(false, "PATCH"), activityTemplateEnumClause, "", 1)

	readOnly, writable := make(map[string]bool), make(map[string]bool)
	for template, ref := range mapping {
		schema, ok := spec.Components.Schemas[refTypeName(ref)]
		if !ok {
			t.Fatalf("template %q maps to %q, which is not a component schema", template, ref)
		}
		for name, prop := range schema.Properties {
			if prop.ReadOnly {
				readOnly[name] = true
				continue
			}
			writable[name] = true
		}
	}
	// A field carried by several templates is only server-owned if every one of
	// them marks it read-only. Classifying on the first schema seen would pick a
	// side by map-iteration order, so a half-read-only field would flip the
	// exemption between runs instead of failing.
	var fields, serverOwned, conflicting []string
	for name := range readOnly {
		if writable[name] {
			conflicting = append(conflicting, name)
			continue
		}
		serverOwned = append(serverOwned, name)
	}
	if len(conflicting) > 0 {
		t.Fatalf("%v are read-only on one activity template and writable on another - the spec has to pick one", sortedClone(conflicting))
	}
	for name := range writable {
		fields = append(fields, name)
	}
	if len(fields) == 0 {
		t.Fatal("the activity content schemas carry no properties - the committed openapi.yaml is behind the server")
	}

	for _, name := range sortedClone(fields) {
		if !mentionsField(desc, name) {
			t.Errorf("activity content field %q is in the spec but absent from the content_json description", name)
		}
	}

	// log_backlog is exempt from the sweep but still documented, as a read-only
	// field an agent fetches rather than sends; the other two are invisible on
	// purpose.
	want := []string{"log_backlog", "snoozed_until", "warning_pushed"}
	if got := sortedClone(serverOwned); !slices.Equal(got, want) {
		t.Errorf("the spec marks %v read-only, the exemption list is %v", got, want)
	}
	// The exempt fields also have to stay out of the description: naming one
	// invites a write the server strips. log_backlog is the deliberate
	// exception, documented as something to fetch rather than send.
	for _, name := range sortedClone(serverOwned) {
		if name == "log_backlog" {
			continue
		}
		if mentionsField(desc, name) {
			t.Errorf("server-owned field %q is named in the content_json description", name)
		}
	}
}

// Every property only the media schema carries (the player fields and their
// controls) has to be named in the media clauses: like the image trio, the
// server 422s them on any other template, so an agent that learns them from
// the spec but not from the description sends a request the wrong way round.
// The player fields and the control slots are checked against their own clause,
// because favorite is both and volume is a prefix of two slots - one combined
// clause matched them whichever half was deleted.
func TestActivityMediaFieldParity(t *testing.T) {
	spec := apiSpec(t)
	mapping := activityContentMapping(t, spec)
	mediaRef, ok := mapping["media"]
	if !ok {
		t.Fatal("the spec mapping carries no media template - the committed openapi.yaml is behind the server")
	}
	media, ok := spec.Components.Schemas[refTypeName(mediaRef)]
	if !ok {
		t.Fatalf("media maps to %q, which is not a component schema", mediaRef)
	}
	shared := make(map[string]bool)
	for template, ref := range mapping {
		if template == "media" {
			continue
		}
		schema, ok := spec.Components.Schemas[refTypeName(ref)]
		if !ok {
			continue
		}
		for name := range schema.Properties {
			shared[name] = true
		}
	}
	var mediaOnly []string
	for name := range media.Properties {
		if !shared[name] {
			mediaOnly = append(mediaOnly, name)
		}
	}
	slices.Sort(mediaOnly)
	if len(mediaOnly) == 0 {
		t.Fatal("ContentMedia carries no media-only properties - the committed openapi.yaml is behind the server")
	}

	t.Run("media-only fields", func(t *testing.T) {
		// Matched against the player clause alone, not the controls one: favorite
		// is a player field and a control slot, and volume is a prefix of the
		// volume_up/volume_down slots, so both still matched a combined clause
		// after their own documentation was deleted.
		for _, name := range mediaOnly {
			clause := activityMediaClause
			if name == "controls" {
				// controls opens its own clause rather than sitting in the field list.
				clause = activityMediaControlsClause
			}
			if !strings.Contains(clause, name) {
				t.Errorf("media-only field %q is in the spec but absent from the media clause: %s", name, clause)
			}
		}
	})

	t.Run("playback_state enum", func(t *testing.T) {
		state, ok := media.Properties["playback_state"]
		if !ok || len(state.Enum) == 0 {
			t.Fatal("ContentMedia.playback_state carries no enum")
		}
		if listed, want := sortedClone(mediaPlaybackStates), sortedClone(state.Enum); !slices.Equal(listed, want) {
			t.Errorf("the description offers playback states %v, the spec enum is %v", listed, want)
		}
	})

	t.Run("seconds caps", func(t *testing.T) {
		// position_seconds and duration_seconds share the 7-day ceiling, and the
		// clause states it as a number. The spec carried the position_seconds cap
		// for a release while the description said nothing, which is the drift
		// this pins.
		for _, field := range []string{"position_seconds", "duration_seconds"} {
			prop, ok := media.Properties[field]
			if !ok {
				t.Errorf("ContentMedia carries no %s - the committed openapi.yaml is behind the server", field)
				continue
			}
			if prop.Maximum == nil {
				t.Errorf("%s carries no maximum in the spec, so the cap the description states is unpinned", field)
				continue
			}
			if *prop.Maximum != float64(mediaSecondsMax) {
				t.Errorf("the description caps %s at %d, the spec maximum is %s", field, mediaSecondsMax, formatBound(*prop.Maximum))
			}
		}
	})

	t.Run("control slots", func(t *testing.T) {
		controls, ok := media.Properties["controls"]
		if !ok || len(controls.Properties) == 0 {
			t.Fatal("ContentMedia.controls carries no properties")
		}
		// Every slot except the extra array has to be named; extra is documented
		// as a shape rather than a slot.
		for name := range controls.Properties {
			if !strings.Contains(activityMediaControlsClause, name) {
				t.Errorf("control slot %q is in the spec but absent from the controls clause: %s", name, activityMediaControlsClause)
			}
		}
	})
}

func sortedClone(values []string) []string {
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	return sorted
}

// The image trio is accepted on the generic, media and steps templates only -
// the server 422s it anywhere else - so a description that names the fields without
// the restriction sends agents into a rejected request. The field names are
// prose; the template list, the shape enum and both caps are the named lists the
// clause is built from, checked here against a spec that moves - the same drift
// the widget enum hit.
func TestActivityImageFieldParity(t *testing.T) {
	spec := apiSpec(t)
	mapping := activityContentMapping(t, spec)

	// Which templates accept an image, which fields they take, and the bounds on
	// those fields, all read off the spec rather than restated here - restating is
	// what drifts.
	fields := make(map[string]bool)
	maxLengths := make(map[string]int)
	var shapes []string
	var templates []string
	for template, ref := range mapping {
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
		if !strings.Contains(activityImageClause, name) {
			t.Errorf("activity content field %q is in the spec but absent from the image clause", name)
		}
	}
	// Scanned over the whole description, not just the image clause: an image
	// field named anywhere else is advertised just as loudly.
	for _, name := range regexp.MustCompile(`image_[a-z_]+`).FindAllString(desc, -1) {
		if !fields[name] {
			t.Errorf("the content_json description advertises %q, which no activity content schema carries", name)
		}
	}

	if listed := sortedClone(imageTemplates); !slices.Equal(listed, templates) {
		t.Errorf("the image clause lists templates %v, the spec carries image fields on %v", listed, templates)
	}
	if len(shapes) == 0 {
		t.Error("image_shape carries no enum in the spec, so the shapes the description offers are unpinned")
	} else if listed := sortedClone(imageShapes); !slices.Equal(listed, shapes) {
		t.Errorf("the description offers shapes %v, the spec enum is %v", listed, shapes)
	}

	// A cap the description overstates is a request the agent builds and the
	// server rejects, so both numbers come from the spec too.
	for _, stated := range []struct {
		field string
		limit int
	}{
		{"image_url", imageURLMaxLength},
		{"image_thumbhash", imageThumbhashMaxLength},
	} {
		want, ok := maxLengths[stated.field]
		if !ok {
			t.Errorf("%s carries no maxLength in the spec, so the cap the description states is unpinned", stated.field)
			continue
		}
		if stated.limit != want {
			t.Errorf("the description caps %s at %d, the spec maxLength is %d", stated.field, stated.limit, want)
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
	// The board/log/media templates must be advertised in the enum and documented.
	if !strings.Contains(aPatch, "timeline|board|log|media)") {
		t.Errorf("activity desc missing board/log/media in template enum: %s", aPatch)
	}
	if !strings.Contains(aPatch, "board (tiles") || !strings.Contains(aPatch, "log (lines") || !strings.Contains(aPatch, "media (") {
		t.Errorf("activity desc missing board/log/media field docs: %s", aPatch)
	}
	// Media fields are activity-only, and the widget description must not
	// advertise a player card widgets cannot render.
	for _, mediaOnly := range []string{"media_title", "playback_state", "controls"} {
		if strings.Contains(wPost, mediaOnly) {
			t.Errorf("widget desc should not mention activity media field %q: %s", mediaOnly, wPost)
		}
	}
	// The per-field sweep, in both directions, lives in
	// TestActivityContentFieldParity: it derives the names from the spec instead
	// of hand-listing them, and matches on word boundaries rather than
	// substrings.
}
