package tools

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"gopkg.in/yaml.v3"

	"github.com/mac-lucky/pushward-mcp/internal/client"
)

// TestSkillToolNames holds the agent skill under skills/ to the registered
// tools. Agents call what the skill shows, so a renamed tool or argument
// would fail in someone else's session with an error nobody traces back to
// the docs.
//
// Every snake_case code span in prose must be a tool, a tool argument, or a
// field the API spec defines. Every example call, in a code block or inline,
// must name a tool the hosted server has, use real arguments with values of
// the right type, include the required ones, and carry content_json and
// actions whose keys the API knows.
func TestSkillToolNames(t *testing.T) {
	api := client.NewAPIClient("http://127.0.0.1:1", "tok")
	hosted := mcpserver.NewMCPServer("pushward-test", "0.0.0")
	RegisterAll(hosted, api, nil)
	local := mcpserver.NewMCPServer("pushward-test", "0.0.0")
	RegisterAll(local, api, client.NewRelayClient("http://127.0.0.1:1", "tok"))

	// A snake_case name in prose is a tool, a tool argument, a content or
	// action field from the API spec, or the answer's action_id.
	spec := loadSkillSpec(t)
	known := map[string]bool{"action_id": true}
	for name, st := range local.ListTools() {
		known[name] = true
		for arg := range inputSchema(t, st.Tool).Properties {
			known[arg] = true
		}
	}
	for _, fields := range []map[string]bool{spec.activityFields, spec.widgetFields, spec.actionFields} {
		maps.Copy(known, fields)
	}

	const dir = "../../skills"
	var docs []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".md") {
			docs = append(docs, path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) == 0 {
		t.Fatalf("no skill files under %s", dir)
	}

	for _, path := range docs {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		name, _ := filepath.Rel(dir, path)
		isSkill := filepath.Base(path) == "SKILL.md"
		if isSkill {
			checkSkillFrontmatter(t, name, filepath.Base(filepath.Dir(path)), text)
		}

		blocks, spans := splitFences(text)
		calls := 0
		for _, span := range spans {
			if c := exampleCalls(span); len(c) > 0 {
				calls += len(c)
				for _, call := range c {
					checkCall(t, name, hosted, spec, call)
				}
				continue
			}
			if snakeCase.MatchString(span) && !known[span] {
				t.Errorf("%s: `%s` is neither a tool nor a tool argument", name, span)
			}
		}
		for _, block := range blocks {
			for _, call := range exampleCalls(block) {
				calls++
				checkCall(t, name, hosted, spec, call)
			}
		}
		if isSkill && calls == 0 {
			t.Errorf("%s: found no example tool calls; is the parser still matching?", name)
		}
	}
}

var snakeCase = regexp.MustCompile(`^[a-z]+(_[a-z0-9]+)+$`)

type exampleCall struct {
	tool string
	args string
}

var callStart = regexp.MustCompile(`^([a-z][a-z_]+) (\{.*)$`)

// exampleCalls finds `tool_name {json}` lines. The JSON may continue over
// the following lines until its braces balance.
func exampleCalls(block string) []exampleCall {
	var out []exampleCall
	lines := strings.Split(block, "\n")
	for i := 0; i < len(lines); i++ {
		m := callStart.FindStringSubmatch(strings.TrimSpace(lines[i]))
		if m == nil {
			continue
		}
		args := m[2]
		for depth(args) > 0 && i+1 < len(lines) {
			i++
			args += "\n" + lines[i]
		}
		out = append(out, exampleCall{m[1], args})
	}
	return out
}

// depth counts unclosed braces and brackets outside JSON strings.
func depth(s string) int {
	d := 0
	in, esc := false, false
	for _, r := range s {
		switch {
		case esc:
			esc = false
		case in && r == '\\':
			esc = true
		case r == '"':
			in = !in
		case in:
		case r == '{' || r == '[':
			d++
		case r == '}' || r == ']':
			d--
		}
	}
	return d
}

func checkCall(t *testing.T, file string, s *mcpserver.MCPServer, spec skillSpec, c exampleCall) {
	t.Helper()
	tool := s.GetTool(c.tool)
	if tool == nil {
		t.Errorf("%s: %s is not a tool on the hosted server", file, c.tool)
		return
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(c.args), &args); err != nil {
		t.Errorf("%s: %s arguments are not a JSON object: %v", file, c.tool, err)
		return
	}
	schema := inputSchema(t, tool.Tool)
	for k, v := range args {
		prop, ok := schema.Properties[k].(map[string]any)
		if !ok {
			t.Errorf("%s: %s has no argument %q", file, c.tool, k)
			continue
		}
		if err := checkValue(prop, v); err != nil {
			t.Errorf("%s: %s %s: %v", file, c.tool, k, err)
		}
	}
	for _, k := range schema.Required {
		if _, ok := args[k]; !ok {
			t.Errorf("%s: %s example leaves out required argument %q", file, c.tool, k)
		}
	}

	if raw, ok := args["content_json"]; ok {
		str, _ := raw.(string)
		if !isJSONObject(str) {
			t.Errorf("%s: %s content_json is not a JSON object string", file, c.tool)
			return
		}
		var content map[string]any
		_ = json.Unmarshal([]byte(str), &content)
		fields, templates := spec.activityFields, spec.activityTemplates
		if strings.HasSuffix(c.tool, "_widget") {
			fields, templates = spec.widgetFields, spec.widgetTemplates
		}
		for k := range content {
			if !fields[k] {
				t.Errorf("%s: %s content_json has unknown field %q", file, c.tool, k)
			}
		}
		if tmpl, ok := content["template"].(string); ok && !templates[tmpl] {
			t.Errorf("%s: %s content_json has unknown template %q", file, c.tool, tmpl)
		}
	}
	if actions, ok := args["actions"].([]any); ok {
		for _, a := range actions {
			m, _ := a.(map[string]any)
			for k := range m {
				if !spec.actionFields[k] {
					t.Errorf("%s: %s action has unknown field %q", file, c.tool, k)
				}
			}
		}
	}
}

// checkValue applies the parts of a property schema an example can get
// wrong: type, enum and numeric bounds.
func checkValue(prop map[string]any, v any) error {
	typ, _ := prop["type"].(string)
	var ok bool
	switch typ {
	case "string":
		_, ok = v.(string)
	case "number":
		_, ok = v.(float64)
	case "integer":
		f, isNum := v.(float64)
		ok = isNum && f == math.Trunc(f)
	case "boolean":
		_, ok = v.(bool)
	case "object":
		_, ok = v.(map[string]any)
	case "array":
		_, ok = v.([]any)
	default:
		ok = true
	}
	if !ok {
		return fmt.Errorf("want a %s, got %v", typ, v)
	}
	if enum := enumValues(prop["enum"]); len(enum) > 0 && !slices.Contains(enum, fmt.Sprint(v)) {
		return fmt.Errorf("%v is not one of %v", v, enum)
	}
	if f, isNum := v.(float64); isNum {
		if lo, ok := number(prop["minimum"]); ok && f < lo {
			return fmt.Errorf("%v is below the minimum %v", f, lo)
		}
		if hi, ok := number(prop["maximum"]); ok && f > hi {
			return fmt.Errorf("%v is above the maximum %v", f, hi)
		}
	}
	return nil
}

// number reads a schema bound; mcp.Min and mcp.Max keep whatever numeric type
// the tool definition passed them.
func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

func enumValues(e any) []string {
	var out []string
	switch e := e.(type) {
	case []string:
		out = e
	case []any:
		for _, v := range e {
			out = append(out, fmt.Sprint(v))
		}
	}
	return out
}

func inputSchema(t *testing.T, tool mcp.Tool) mcp.ToolArgumentsSchema {
	t.Helper()
	if tool.RawInputSchema == nil {
		return mcp.ToolArgumentsSchema(tool.InputSchema)
	}
	var schema mcp.ToolArgumentsSchema
	if err := json.Unmarshal(tool.RawInputSchema, &schema); err != nil {
		t.Fatalf("%s raw input schema: %v", tool.Name, err)
	}
	return schema
}

// skillSpec is what the embedded API spec says content_json and actions may
// contain.
type skillSpec struct {
	activityFields, activityTemplates map[string]bool
	widgetFields, widgetTemplates     map[string]bool
	actionFields                      map[string]bool
}

func loadSkillSpec(t *testing.T) skillSpec {
	t.Helper()
	data, err := os.ReadFile("../docs/assets/api-openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					Enum []string `yaml:"enum"`
				} `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	spec := skillSpec{
		activityFields: map[string]bool{}, activityTemplates: map[string]bool{},
		widgetFields: map[string]bool{}, widgetTemplates: map[string]bool{},
		actionFields: map[string]bool{},
	}
	for name, schema := range doc.Components.Schemas {
		if tmpl, ok := strings.CutPrefix(name, "Content"); ok {
			spec.activityTemplates[strings.ToLower(tmpl)] = true
			for k := range schema.Properties {
				spec.activityFields[k] = true
			}
		}
	}
	for k, p := range doc.Components.Schemas["WidgetContent"].Properties {
		spec.widgetFields[k] = true
		if k == "template" {
			for _, v := range p.Enum {
				spec.widgetTemplates[v] = true
			}
		}
	}
	for k := range doc.Components.Schemas["NotificationAction"].Properties {
		spec.actionFields[k] = true
	}
	if len(spec.activityTemplates) < 10 || len(spec.widgetTemplates) < 10 || len(spec.actionFields) == 0 {
		t.Fatalf("api-openapi.yaml no longer has the expected Content*, WidgetContent and NotificationAction schemas: %d activity templates, %d widget templates, %d action fields",
			len(spec.activityTemplates), len(spec.widgetTemplates), len(spec.actionFields))
	}
	return spec
}

// splitFences returns the fenced code blocks and the inline code spans in
// the prose around them.
func splitFences(text string) (blocks, spans []string) {
	var p, b strings.Builder
	in := false
	for line := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if in {
				blocks = append(blocks, b.String())
				b.Reset()
			}
			in = !in
			continue
		}
		if in {
			b.WriteString(line)
			b.WriteByte('\n')
		} else {
			p.WriteString(line)
			p.WriteByte('\n')
		}
	}
	for _, m := range codeSpan.FindAllStringSubmatch(p.String(), -1) {
		spans = append(spans, m[1])
	}
	return blocks, spans
}

var codeSpan = regexp.MustCompile("`([^`]+)`")

var skillNamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// checkSkillFrontmatter applies the Agent Skills rules that skill installers
// enforce: the name matches the directory and the description fits.
func checkSkillFrontmatter(t *testing.T, file, dirName, text string) {
	t.Helper()
	rest, ok := strings.CutPrefix(text, "---\n")
	head, _, ok2 := strings.Cut(rest, "\n---\n")
	if !ok || !ok2 {
		t.Errorf("%s: no frontmatter", file)
		return
	}
	var fm struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal([]byte(head), &fm); err != nil {
		t.Errorf("%s: frontmatter: %v", file, err)
		return
	}
	if fm.Name != dirName || !skillNamePattern.MatchString(fm.Name) || len(fm.Name) > 64 {
		t.Errorf("%s: name %q must be the directory name %q, lowercase words joined by hyphens, at most 64 characters", file, fm.Name, dirName)
	}
	if fm.Description == "" || len(fm.Description) > 1024 {
		t.Errorf("%s: description is %d characters, want 1 to 1024", file, len(fm.Description))
	}
	if strings.ContainsAny(fm.Description, "<>") {
		t.Errorf("%s: description must not contain angle brackets", file)
	}
}
