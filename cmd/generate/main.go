// Command generate reads both OpenAPI specs and emits typed MCP tool
// definitions into internal/tools/api_gen.go and internal/tools/relay_gen.go.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/format"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"
)

// ---------- minimal OpenAPI 3.1 AST ----------

type openAPISpec struct {
	Paths      map[string]pathItem `json:"paths" yaml:"paths"`
	Components struct {
		Schemas map[string]schemaObj `json:"schemas" yaml:"schemas"`
	} `json:"components" yaml:"components"`
}

type pathItem map[string]operation // "get", "post", etc.

type operation struct {
	OperationID string       `json:"operationId" yaml:"operationId"`
	Summary     string       `json:"summary" yaml:"summary"`
	Description string       `json:"description" yaml:"description"`
	RequestBody *requestBody `json:"requestBody" yaml:"requestBody"`
	Security    []any        `json:"security" yaml:"security"`
}

type requestBody struct {
	Required bool                       `json:"required" yaml:"required"`
	Content  map[string]mediaTypeObject `json:"content" yaml:"content"`
}

type mediaTypeObject struct {
	Schema schemaObj `json:"schema" yaml:"schema"`
}

type schemaObj struct {
	Type                 any                  `json:"type" yaml:"type"` // string or []string
	Ref                  string               `json:"$ref" yaml:"$ref"`
	Properties           map[string]schemaObj `json:"properties" yaml:"properties"`
	Required             []string             `json:"required" yaml:"required"`
	Items                *schemaObj           `json:"items" yaml:"items"`
	Enum                 []string             `json:"enum" yaml:"enum"`
	Format               string               `json:"format" yaml:"format"`
	Maximum              *float64             `json:"maximum" yaml:"maximum"`
	Minimum              *float64             `json:"minimum" yaml:"minimum"`
	MaxLength            *int                 `json:"maxLength" yaml:"maxLength"`
	MinLength            *int                 `json:"minLength" yaml:"minLength"`
	Pattern              string               `json:"pattern" yaml:"pattern"`
	ReadOnly             bool                 `json:"readOnly" yaml:"readOnly"`
	Desc                 string               `json:"description" yaml:"description"`
	Examples             []any                `json:"examples" yaml:"examples"`
	AdditionalProperties any                  `json:"additionalProperties" yaml:"additionalProperties"`
}

// ---------- code generation models ----------

type toolDef struct {
	Name        string // MCP tool name (snake_case)
	FuncName    string // Go function name
	Description string
	Method      string
	Path        string
	PathParams  []paramDef
	Params      []paramDef
	HasBody     bool
	ContentJSON bool   // true if body is passed as a single content_json string
	ContentDesc string // description for the content_json param (widget vs activity, POST vs PATCH)
	IsRelay     bool
	Provider    string // relay provider name
	PayloadJSON bool   // relay: entire body as payload_json
}

type paramDef struct {
	Name      string
	GoName    string
	MCPType   string // "String", "Number", "Boolean", "Object", "Array"
	Desc      string
	Required  bool
	Enum      []string
	GoType    string // Go type used in client struct for Object/Array params (e.g. "*client.MediaAttachment", "[]client.NotificationAction")
	ItemsType string // Item type description (used for array property items schema)
	Opaque    bool   // forward as json.RawMessage instead of typed unmarshal — for fields whose schema drifts faster than the MCP rebuilds
}

// opaqueArrayFields lists request-body field names that must be forwarded as
// raw JSON instead of unmarshalled into a typed slice. The server is the
// source of truth for these schemas — typed parsing silently dropped unknown
// fields the server had added since the last MCP rebuild (see commit 33912d9).
var opaqueArrayFields = map[string]bool{
	"actions": true,
}

// Live OpenAPI spec URLs.
const (
	apiSpecURL   = "https://api.pushward.app/openapi.json"
	relaySpecURL = "https://relay.pushward.app/openapi.json"
)

// Published reference docs embedded into the binary and served by the
// get_pushward_docs tool. Fetched at generate time (see refreshAsset). The API
// spec is fetched as YAML here — distinct from apiSpecURL's JSON used for tool
// generation — so the embedded copy matches what api.pushward.app serves.
const (
	llmsIndexURL     = "https://pushward.app/llms.txt"
	llmsFullURL      = "https://pushward.app/llms-full.txt"
	apiSpecYAMLURL   = "https://api.pushward.app/openapi.yaml"
	relaySpecJSONURL = "https://relay.pushward.app/openapi.json"
)

// ---------- main ----------

func main() {
	rootDir := findRootDir()
	outDir := filepath.Join(rootDir, "internal", "tools")
	// Any non-empty value opts in (so PUSHWARD_USE_LOCAL_SPEC=0 still means
	// "use local"). Skip the network entirely when set — useful for testing
	// spec changes that haven't been deployed yet.
	useLocal := os.Getenv("PUSHWARD_USE_LOCAL_SPEC") != ""

	apiData := loadSpec(useLocal, apiSpecURL, filepath.Join(rootDir, "openapi.yaml"))
	apiSpec := parseSpecJSON(apiData, "api")
	apiTools := buildAPITools(apiSpec)

	relayData := loadSpec(useLocal, relaySpecURL, filepath.Join(rootDir, "relay-openapi.json"))
	relaySpec := parseSpecJSON(relayData, "relay")
	relayTools := buildRelayTools(relaySpec)

	// Generate files
	writeGenFile(filepath.Join(outDir, "api_gen.go"), apiToolsTemplate, apiTools)
	writeGenFile(filepath.Join(outDir, "relay_gen.go"), relayToolsTemplate, relayTools)

	// Refresh the reference docs embedded into the binary and served by the
	// get_pushward_docs tool. Additive to tool generation. best-practices.md is
	// hand-authored and intentionally absent here, so it is never overwritten.
	assetsDir := filepath.Join(rootDir, "internal", "docs", "assets")
	if err := os.MkdirAll(assetsDir, 0o750); err != nil {
		fmt.Fprintf(os.Stderr, "creating %s: %v\n", assetsDir, err)
		os.Exit(1)
	}
	refreshAsset(useLocal, llmsIndexURL, filepath.Join(assetsDir, "llms.txt"))
	refreshAsset(useLocal, llmsFullURL, filepath.Join(assetsDir, "llms-full.txt"))
	refreshAsset(useLocal, apiSpecYAMLURL, filepath.Join(assetsDir, "api-openapi.yaml"))
	refreshAsset(useLocal, relaySpecJSONURL, filepath.Join(assetsDir, "relay-openapi.json"))

	fmt.Printf("Generated %d API tools and %d relay tools\n", len(apiTools), len(relayTools))
}

// refreshAsset fetches url and overwrites destPath with the response, refreshing
// a reference doc embedded into the binary. On fetch failure — or when useLocal
// is set — it logs and leaves the committed asset in place (offline-safe: the
// embedded copy is the fallback). A write failure is fatal: it means a broken
// source tree.
func refreshAsset(useLocal bool, url, destPath string) {
	if useLocal {
		fmt.Fprintf(os.Stderr, "PUSHWARD_USE_LOCAL_SPEC set, keeping %s\n", destPath)
		return
	}
	data, err := fetchURL(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "refresh %s failed (%v); keeping committed %s\n", url, err, destPath)
		return
	}
	if err := os.WriteFile(destPath, data, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", destPath, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "refreshed %s (%d bytes)\n", destPath, len(data))
}

func findRootDir() string {
	// Walk up from cmd/generate to find go.mod
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			fmt.Fprintln(os.Stderr, "cannot find go.mod")
			os.Exit(1)
		}
		dir = parent
	}
}

// loadSpec returns spec bytes from either the live URL (with on-failure
// fallback to fallbackPath) or directly from fallbackPath when useLocal is
// set. Exits the process if the local read fails.
func loadSpec(useLocal bool, url, fallbackPath string) []byte {
	if !useLocal {
		if data, err := fetchURL(url); err == nil {
			return data
		}
		fmt.Fprintf(os.Stderr, "fetch %s failed, falling back to %s\n", url, fallbackPath)
	} else {
		fmt.Fprintf(os.Stderr, "PUSHWARD_USE_LOCAL_SPEC set, reading %s\n", fallbackPath)
	}
	// fallbackPath is a build-time constant (the vendored OpenAPI spec), not user
	// input — this is a code generator, not a runtime path.
	data, err := os.ReadFile(fallbackPath) // #nosec G304
	if err != nil {
		fmt.Fprintf(os.Stderr, "reading %s: %v\n", fallbackPath, err)
		os.Exit(1)
	}
	return data
}

func fetchURL(url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "fetched %s (%d bytes)\n", url, len(data))
	return data, nil
}

// parseSpecJSON parses JSON or YAML OpenAPI spec data.
func parseSpecJSON(data []byte, name string) *openAPISpec {
	var spec openAPISpec
	// Try JSON first
	if err := json.Unmarshal(data, &spec); err != nil {
		// Fall back to YAML
		if yamlErr := yaml.Unmarshal(data, &spec); yamlErr != nil {
			fmt.Fprintf(os.Stderr, "parsing %s spec: json: %v, yaml: %v\n", name, err, yamlErr)
			os.Exit(1)
		}
	}
	return &spec
}

// ---------- API tool building ----------

// skipOperations lists operationIDs that are handled as composite tools
// in composite.go instead of being generated, plus deprecated endpoints
// that survived in the live OpenAPI spec but are gone from the public
// surface post-merge-patch cleanup.
var skipOperations = map[string]bool{
	"listActivities":     true, // enhanced with state/source filtering and summary mode
	"getActivity":        true, // superseded by a composite that adds include_log_backlog (?include=log_backlog)
	"setActivityAlarm":   true, // removed from public surface — alarm is now a merge-patch field
	"clearActivityAlarm": true, // same
}

func buildAPITools(spec *openAPISpec) []toolDef {
	var tools []toolDef

	for path, item := range spec.Paths {
		for method, op := range item {
			if op.OperationID == "" {
				continue
			}
			if skipOperations[op.OperationID] {
				continue
			}
			t := toolDef{
				Name:        toSnakeCase(op.OperationID),
				FuncName:    toPascalCase(op.OperationID),
				Description: firstNonEmpty(op.Summary, op.Description),
				Method:      strings.ToUpper(method),
				Path:        path,
			}

			// Extract path parameters
			t.PathParams = extractPathParams(path)

			// Extract request body parameters. Huma serves PATCH endpoints
			// with `application/merge-patch+json` (RFC 7396) — accept both so
			// PATCH operations don't lose their body params.
			if op.RequestBody != nil {
				ct, ok := op.RequestBody.Content["application/json"]
				if !ok {
					ct, ok = op.RequestBody.Content["application/merge-patch+json"]
				}
				if ok {
					schema := resolveRef(spec, ct.Schema)
					t.HasBody = true
					t.Params = schemaToParams(spec, schema, op.RequestBody.Required)

					// If the schema has a "content" field that is a complex object,
					// use content_json approach. The description differs by content
					// type (WidgetContent vs the activity discriminated oneOf) and by
					// method (PATCH uses merge-patch semantics; POST requires the full
					// object), so resolve it per-tool rather than hardcoding one string.
					if cp, hasContent := schema.Properties["content"]; hasContent {
						t.ContentJSON = true
						t.ContentDesc = contentJSONDesc(refTypeName(cp.Ref) == widgetContentSchema, t.Method)
					}
				}
			}

			tools = append(tools, t)
		}
	}

	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return tools
}

func extractPathParams(path string) []paramDef {
	re := regexp.MustCompile(`\{(\w+)\}`)
	matches := re.FindAllStringSubmatch(path, -1)
	var params []paramDef
	for _, m := range matches {
		params = append(params, paramDef{
			Name:     m[1],
			GoName:   toPascalCase(m[1]),
			MCPType:  "String",
			Desc:     m[1] + " path parameter",
			Required: true,
		})
	}
	return params
}

func schemaToParams(spec *openAPISpec, schema schemaObj, bodyRequired bool) []paramDef {
	var params []paramDef
	requiredSet := make(map[string]bool)
	for _, r := range schema.Required {
		requiredSet[r] = true
	}

	// Sorted property names for deterministic output
	propNames := make([]string, 0, len(schema.Properties))
	for name := range schema.Properties {
		propNames = append(propNames, name)
	}
	sort.Strings(propNames)

	for _, name := range propNames {
		raw := schema.Properties[name]
		if raw.ReadOnly {
			continue
		}

		// Skip $schema meta fields
		if name == "$schema" {
			continue
		}

		// Capture ref info before resolving so we can name nested types.
		propRef := raw.Ref
		var itemsRef string
		if raw.Items != nil {
			itemsRef = raw.Items.Ref
		}
		prop := resolveRef(spec, raw)

		// Skip the `content` field — it's always emitted via the content_json
		// string path (see buildAPITools). This applies whether the schema is a
		// nested object, a $ref, or a discriminator/oneOf — the fallback
		// `schemaType` would otherwise mislabel oneOf as "string" and emit a
		// duplicate `Content` field in the input struct literal.
		if name == "content" {
			continue
		}

		// Skip free-form object fields (e.g., metadata: map[string]string) without a typed schema —
		// not representable as a typed MCP object param.
		if propRef == "" && schemaType(prop) == "object" && len(prop.Properties) == 0 {
			continue
		}

		// Property-level description wins over the resolved schema's description
		// (a $ref'd schema's description is generic, the property-level one is contextual).
		desc := raw.Desc
		if desc == "" {
			desc = prop.Desc
		}

		p := paramDef{
			Name:     name,
			GoName:   toPascalCase(name),
			Desc:     desc,
			Required: requiredSet[name] && bodyRequired,
			Enum:     prop.Enum,
		}

		// Handle ref'd object schemas as MCP object params, mapped to a hand-defined Go struct.
		if propRef != "" && schemaType(prop) == "object" {
			p.MCPType = "Object"
			p.GoType = "*client." + refTypeName(propRef)
			if p.Desc == "" {
				p.Desc = name
			}
			params = append(params, p)
			continue
		}

		// Handle array-of-ref'd-object schemas as MCP array params.
		if schemaType(prop) == "array" && itemsRef != "" {
			p.MCPType = "Array"
			if opaqueArrayFields[name] {
				p.Opaque = true
				p.GoType = "json.RawMessage"
			} else {
				p.GoType = "[]client." + refTypeName(itemsRef)
			}
			p.ItemsType = refTypeName(itemsRef)
			if p.Desc == "" {
				p.Desc = name
			}
			params = append(params, p)
			continue
		}

		// Skip remaining object-typed fields (free-form maps with additionalProperties).
		if schemaType(prop) == "object" || (prop.AdditionalProperties != nil && len(prop.Properties) == 0) {
			continue
		}

		// Skip array fields whose items are not ref'd schemas — no typed Go target available.
		if schemaType(prop) == "array" {
			continue
		}

		switch schemaType(prop) {
		case "integer", "number":
			p.MCPType = "Number"
			if p.Desc == "" {
				p.Desc = name
			}
			// Add range info to description
			if prop.Minimum != nil || prop.Maximum != nil {
				rangeStr := ""
				if prop.Minimum != nil {
					rangeStr += "min: " + formatBound(*prop.Minimum)
				}
				if prop.Maximum != nil {
					if rangeStr != "" {
						rangeStr += ", "
					}
					rangeStr += "max: " + formatBound(*prop.Maximum)
				}
				p.Desc += " (" + rangeStr + ")"
			}
		case "boolean":
			p.MCPType = "Boolean"
		default:
			p.MCPType = "String"
		}

		if p.Desc == "" {
			p.Desc = name
		}

		params = append(params, p)
	}
	return params
}

// widgetContentSchema is the OpenAPI schema name whose `content` field carries
// widget (not activity) shape; named so a server-side rename surfaces here rather
// than silently flipping every widget tool to the activity description.
const widgetContentSchema = "WidgetContent"

// mergePatchNote is the shared wording for PATCH endpoints, kept in one place so
// the activity and widget descriptions can't drift apart.
const mergePatchNote = "PATCH applies RFC 7396 JSON Merge Patch semantics — only send the fields you want to change, null clears a field, absent preserves. "

// contentJSONDesc returns the description for a tool's content_json parameter.
// The activity and widget content schemas are disjoint (different template enums,
// both additionalProperties:false), so a single shared string misleads agents into
// sending the wrong shape — which the server then rejects. POST create endpoints
// require the full content object; PATCH endpoints apply RFC 7396 merge-patch.
func contentJSONDesc(isWidget bool, method string) string {
	patch := method == http.MethodPatch
	if isWidget {
		lead := "Widget content as a JSON object. "
		if patch {
			lead += mergePatchNote
		} else {
			lead += "Send the full content object (template is required). "
		}
		return lead + "Fields: template (value|progress|status|gauge|stat_list — selects the visual style), value (number), label, unit, trend (up|down|flat), severity, min_value, max_value, stat_rows (array of stat rows, used by stat_list), icon, subtitle, accent_color, background_color, text_color, tap_action ({url}), url_action, secondary_url_action."
	}
	lead := "Activity content as JSON object. "
	if patch {
		lead += mergePatchNote
	}
	return lead + "Fields: template (generic|countdown|steps|alert|gauge|timeline|board|log), progress (0.0-1.0), state, icon, subtitle, accent_color, background_color, text_color. Template-specific: countdown (duration as integer seconds (60) or duration string (\"60s\", \"1h30m\"), end_date [unix timestamp], warning_threshold, completion_message, alarm, snooze_seconds (60-3600, default 300; how far the /snooze action and iOS AlarmKit snooze extend the timer, only with alarm); if both duration and end_date are sent, end_date wins), steps (current_step, total_steps, step_labels), alert (severity: critical|warning|info, fired_at), gauge (value, min_value, max_value, unit), timeline (value as {key:number}, history as {key:[{timestamp,value}]}, scale, thresholds), board (tiles: 1-4 labeled tiles, each {label, value [string], unit, icon, color, trend [up|down|flat], url_action {url}}, replaced wholesale per update), log (lines: 1-20 newest-first entries, each {text, at [unix seconds], level [info|warn|error]}, replaced wholesale per update; the server also keeps a read-only rolling log_backlog, fetch it via get_activity include_log_backlog)."
}

func resolveRef(spec *openAPISpec, s schemaObj) schemaObj {
	if s.Ref == "" {
		return s
	}
	// Parse #/components/schemas/Foo
	parts := strings.Split(s.Ref, "/")
	if len(parts) < 4 {
		return s
	}
	name := parts[len(parts)-1]
	if resolved, ok := spec.Components.Schemas[name]; ok {
		return resolved
	}
	return s
}

// refTypeName extracts the schema name from a #/components/schemas/Foo $ref.
func refTypeName(ref string) string {
	parts := strings.Split(ref, "/")
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

// formatBound renders a numeric min/max bound without scientific notation, so a
// max of 2592000 reads as "2592000" rather than "2.592e+06". Integral values lose
// their trailing ".0"; fractional values keep their digits.
func formatBound(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func schemaType(s schemaObj) string {
	switch v := s.Type.(type) {
	case string:
		return v
	case []any:
		for _, t := range v {
			if ts, ok := t.(string); ok && ts != "null" {
				return ts
			}
		}
	}
	if s.Format == "int64" || s.Format == "int32" || s.Format == "float" || s.Format == "double" {
		return "number"
	}
	return "string"
}

// ---------- Relay tool building ----------

// Providers where the OpenAPI spec uses flat JSON properties (no $ref nesting).
// These get individual typed parameters.
func isFlat(spec *openAPISpec, schema schemaObj) bool {
	schema = resolveRef(spec, schema)
	if len(schema.Properties) == 0 {
		return false
	}
	// Count non-meta properties and check for $ref
	count := 0
	for name, prop := range schema.Properties {
		if name == "$schema" {
			continue
		}
		if prop.Ref != "" {
			return false // nested object
		}
		if prop.Items != nil && prop.Items.Ref != "" {
			return false // array of nested objects
		}
		count++
	}
	return count <= 12
}

func buildRelayTools(spec *openAPISpec) []toolDef {
	var tools []toolDef

	for path, item := range spec.Paths {
		for _, op := range item {
			if op.OperationID == "" {
				continue
			}

			provider := strings.TrimPrefix(path, "/")
			t := toolDef{
				Name:        "relay_" + provider,
				FuncName:    "Relay" + toPascalCase(provider),
				Description: firstNonEmpty(op.Summary, op.Description),
				IsRelay:     true,
				Provider:    provider,
			}

			if op.RequestBody != nil {
				if ct, ok := op.RequestBody.Content["application/json"]; ok {
					schema := resolveRef(spec, ct.Schema)
					if isFlat(spec, schema) {
						t.Params = schemaToParams(spec, schema, false)
					} else {
						t.PayloadJSON = true
						// Build a description hint from schema
						t.Description += ". Pass the full JSON payload."
					}
				} else {
					// octet-stream (Radarr, Sonarr, Prowlarr)
					t.PayloadJSON = true
					t.Description += ". Pass the full JSON payload (eventType determines the shape)."
				}
			}

			tools = append(tools, t)
		}
	}

	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return tools
}

// ---------- template rendering ----------

var funcMap = template.FuncMap{
	"quote":   func(s string) string { return fmt.Sprintf("%q", s) },
	"hasEnum": func(p paramDef) bool { return len(p.Enum) > 0 },
	"enumList": func(p paramDef) string {
		quoted := make([]string, len(p.Enum))
		for i, e := range p.Enum {
			quoted[i] = fmt.Sprintf("%q", e)
		}
		return strings.Join(quoted, ", ")
	},
	"allParams": func(t toolDef) []paramDef {
		return append(slices.Clone(t.PathParams), t.Params...)
	},
	"pathWithParams": func(t toolDef) string {
		if len(t.PathParams) == 0 {
			return fmt.Sprintf("%q", t.Path)
		}
		// Convert /activities/{slug} to fmt.Sprintf("/activities/%s", slug)
		result := t.Path
		var args []string
		for _, p := range t.PathParams {
			result = strings.ReplaceAll(result, "{"+p.Name+"}", "%s")
			args = append(args, "param"+p.GoName)
		}
		return fmt.Sprintf("fmt.Sprintf(%q, %s)", result, strings.Join(args, ", "))
	},
}

var apiToolsTemplate = template.Must(template.New("api").Funcs(funcMap).Parse(`// Code generated by cmd/generate. DO NOT EDIT.

package tools

import (
	"context"
	"encoding/json"
	"math"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mac-lucky/pushward-mcp/internal/client"
)
{{ define "boolField" -}}
// Send the field only when the caller supplied a real boolean, so an omitted
// (or null) value inherits the server-side default — e.g. push defaults to
// true — instead of being forced to false. RequireBool errors on absent, null,
// or non-bool input; assign only on success. Requires a *bool client field.
if v, err := req.RequireBool({{ quote .Name }}); err == nil {
	input.{{ .GoName }} = &v
}
{{- end }}
func registerAPITools(s *mcpserver.MCPServer, api *client.APIClient) {
{{- range . }}

	// {{ .Name }}
	s.AddTool(
		mcp.NewTool({{ quote .Name }},
			mcp.WithDescription({{ quote .Description }}),
{{- if eq .Method "GET" }}
			mcp.WithReadOnlyHintAnnotation(true),
{{- else if eq .Method "DELETE" }}
			mcp.WithDestructiveHintAnnotation(true),
			mcp.WithIdempotentHintAnnotation(true),
{{- else if or (eq .Method "PATCH") (eq .Method "PUT") }}
			// A merge-patch/replace update rewrites fields but is not data-destroying,
			// and re-applying the same body is a no-op (idempotent). Without these,
			// the unannotated default is destructiveHint:true, which makes clients
			// gate every update behind a confirmation prompt.
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(true),
{{- else if eq .Method "POST" }}
			// Create/send is additive, not destructive; override the destructiveHint:true
			// default so clients don't treat it as a data-clobbering operation.
			mcp.WithDestructiveHintAnnotation(false),
{{- end }}
			// Every tool proxies an external REST API (api.pushward.app), so its
			// results cross a trust boundary — keep the open-world hint explicit.
			mcp.WithOpenWorldHintAnnotation(true),
{{- range .PathParams }}
			mcp.WithString({{ quote .Name }},
				mcp.Required(),
				mcp.Description({{ quote .Desc }}),
			),
{{- end }}
{{- range .Params }}
{{- if eq .MCPType "Array" }}
			mcp.WithArray({{ quote .Name }},
{{- if .Required }}
				mcp.Required(),
{{- end }}
				mcp.Description({{ quote .Desc }}),
				mcp.Items(map[string]any{"type": "object"}),
			),
{{- else if eq .MCPType "Object" }}
			mcp.WithObject({{ quote .Name }},
{{- if .Required }}
				mcp.Required(),
{{- end }}
				mcp.Description({{ quote .Desc }}),
			),
{{- else }}
			mcp.With{{ .MCPType }}({{ quote .Name }},
{{- if .Required }}
				mcp.Required(),
{{- end }}
				mcp.Description({{ quote .Desc }}),
{{- if hasEnum . }}
				mcp.Enum({{ enumList . }}),
{{- end }}
			),
{{- end }}
{{- end }}
{{- if .ContentJSON }}
			mcp.WithString("content_json",
				mcp.Required(),
				mcp.Description({{ quote .ContentDesc }}),
			),
{{- end }}
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handle{{ .FuncName }}(ctx, req, api)
		},
	)
{{- end }}
}
{{ range . }}
func handle{{ .FuncName }}(ctx context.Context, req mcp.CallToolRequest, api *client.APIClient) (*mcp.CallToolResult, error) {
{{- range .PathParams }}
	param{{ .GoName }}, err := req.RequireString({{ quote .Name }})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
{{- end }}
{{- if and .HasBody (not .ContentJSON) }}
{{- range .Params }}
{{- if and .Required (eq .MCPType "String") }}
	param{{ .GoName }}, err := req.RequireString({{ quote .Name }})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
{{- end }}
{{- end }}
	input := client.{{ .FuncName }}Input{
{{- range .Params }}
{{- if and .Required (eq .MCPType "String") }}
		{{ .GoName }}: param{{ .GoName }},
{{- end }}
{{- end }}
	}
{{- range .Params }}
{{- if eq .MCPType "String" }}
{{- if not .Required }}
	if v := req.GetString({{ quote .Name }}, ""); v != "" {
		input.{{ .GoName }} = v
	}
{{- end }}
{{- else if eq .MCPType "Number" }}
{{- if .Required }}
	param{{ .GoName }}, err := req.RequireFloat({{ quote .Name }})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	input.{{ .GoName }} = &param{{ .GoName }}
{{- else }}
	if v := req.GetFloat({{ quote .Name }}, math.NaN()); !math.IsNaN(v) {
		input.{{ .GoName }} = &v
	}
{{- end }}
{{- else if eq .MCPType "Boolean" }}
	{{ template "boolField" . }}
{{- else if eq .MCPType "Object" }}
{{- if .Required }}
	if _, ok := req.GetArguments()[{{ quote .Name }}]; !ok {
		return mcp.NewToolResultError("missing required parameter: {{ .Name }}"), nil
	}
{{- end }}
	if v, ok := req.GetArguments()[{{ quote .Name }}]; ok && v != nil {
		buf, err := json.Marshal(v)
		if err != nil {
			return mcp.NewToolResultError("encoding {{ .Name }}: " + err.Error()), nil
		}
		var parsed {{ .GoType }}
		if err := json.Unmarshal(buf, &parsed); err != nil {
			return mcp.NewToolResultError("parsing {{ .Name }}: " + err.Error()), nil
		}
		input.{{ .GoName }} = parsed
	}
{{- else if eq .MCPType "Array" }}
{{- if .Required }}
	if _, ok := req.GetArguments()[{{ quote .Name }}]; !ok {
		return mcp.NewToolResultError("missing required parameter: {{ .Name }}"), nil
	}
{{- end }}
	if v, ok := req.GetArguments()[{{ quote .Name }}]; ok && v != nil {
		buf, err := json.Marshal(v)
		if err != nil {
			return mcp.NewToolResultError("encoding {{ .Name }}: " + err.Error()), nil
		}
{{- if .Opaque }}
		// Forward opaque JSON — server is the source of truth for the
		// {{ .Name }} schema, so new fields don't require an MCP rebuild.
		input.{{ .GoName }} = json.RawMessage(buf)
{{- else }}
		var parsed {{ .GoType }}
		if err := json.Unmarshal(buf, &parsed); err != nil {
			return mcp.NewToolResultError("parsing {{ .Name }}: " + err.Error()), nil
		}
		input.{{ .GoName }} = parsed
{{- end }}
	}
{{- end }}
{{- end }}
{{- end }}
{{- if .ContentJSON }}
	contentStr, err := req.RequireString("content_json")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if !isJSONObject(contentStr) {
		return mcp.NewToolResultError("content_json must be a JSON object"), nil
	}
{{- range .Params }}
{{- if eq .MCPType "String" }}
{{- if .Required }}
	param{{ .GoName }}, err := req.RequireString({{ quote .Name }})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
{{- end }}
{{- end }}
{{- end }}
	input := client.{{ .FuncName }}Input{
{{- range .Params }}
{{- if and .Required (eq .MCPType "String") }}
		{{ .GoName }}: param{{ .GoName }},
{{- end }}
{{- end }}
		Content: json.RawMessage(contentStr),
	}
{{- range .Params }}
{{- if and (not .Required) (eq .MCPType "String") }}
	if v := req.GetString({{ quote .Name }}, ""); v != "" {
		input.{{ .GoName }} = v
	}
{{- else if and (not .Required) (eq .MCPType "Number") }}
	if v := req.GetFloat({{ quote .Name }}, math.NaN()); !math.IsNaN(v) {
		input.{{ .GoName }} = &v
	}
{{- else if and (not .Required) (eq .MCPType "Boolean") }}
	{{ template "boolField" . }}
{{- end }}
{{- end }}
{{- end }}

{{- if eq .Method "GET" }}
{{- if len .PathParams | eq 0 }}
	raw, err := api.{{ .FuncName }}(ctx)
{{- else }}
	raw, err := api.{{ .FuncName }}(ctx, {{ range .PathParams }}param{{ .GoName }}, {{ end }})
{{- end }}
{{- else if eq .Method "DELETE" }}
	err = api.{{ .FuncName }}(ctx, {{ range .PathParams }}param{{ .GoName }}, {{ end }})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText("deleted successfully"), nil
{{- else }}
{{- if len .PathParams | eq 0 }}
	raw, err := api.{{ .FuncName }}(ctx, input)
{{- else }}
	raw, err := api.{{ .FuncName }}(ctx, {{ range .PathParams }}param{{ .GoName }}, {{ end }}input)
{{- end }}
{{- end }}
{{- if ne .Method "DELETE" }}
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(string(raw)), nil
{{- end }}
}
{{ end }}`))

var relayToolsTemplate = template.Must(template.New("relay").Funcs(funcMap).Parse(`// Code generated by cmd/generate. DO NOT EDIT.

package tools

import (
	"context"
	"encoding/json"
	"math"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mac-lucky/pushward-mcp/internal/client"
)

func registerRelayTools(s *mcpserver.MCPServer, relay *client.RelayClient) {
{{- range . }}

	// {{ .Name }}
	s.AddTool(
		mcp.NewTool({{ quote .Name }},
			mcp.WithDescription({{ quote .Description }}),
{{- if .PayloadJSON }}
			mcp.WithString("payload_json",
				mcp.Required(),
				mcp.Description("Full webhook JSON payload for {{ .Provider }}"),
			),
{{- else }}
{{- range .Params }}
			mcp.With{{ .MCPType }}({{ quote .Name }},
{{- if .Required }}
				mcp.Required(),
{{- end }}
				mcp.Description({{ quote .Desc }}),
{{- if hasEnum . }}
				mcp.Enum({{ enumList . }}),
{{- end }}
			),
{{- end }}
{{- end }}
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handle{{ .FuncName }}(ctx, req, relay)
		},
	)
{{- end }}
}
{{ range . }}
func handle{{ .FuncName }}(ctx context.Context, req mcp.CallToolRequest, relay *client.RelayClient) (*mcp.CallToolResult, error) {
{{- if .PayloadJSON }}
	payloadStr, err := req.RequireString("payload_json")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if !isJSONObject(payloadStr) {
		return mcp.NewToolResultError("payload_json must be a JSON object"), nil
	}
	raw, err := relay.PostWebhook(ctx, {{ quote .Provider }}, json.RawMessage(payloadStr))
{{- else }}
	body := map[string]any{}
{{- range .Params }}
{{- if eq .MCPType "String" }}
	if v := req.GetString({{ quote .Name }}, ""); v != "" {
		body[{{ quote .Name }}] = v
	}
{{- else if eq .MCPType "Number" }}
	if v := req.GetFloat({{ quote .Name }}, math.NaN()); !math.IsNaN(v) {
		body[{{ quote .Name }}] = v
	}
{{- else if eq .MCPType "Boolean" }}
	// Assign on a real boolean (incl. an explicit false) so false is not dropped;
	// RequireBool errors on absent/null, which leaves the field unset.
	if v, err := req.RequireBool({{ quote .Name }}); err == nil {
		body[{{ quote .Name }}] = v
	}
{{- end }}
{{- end }}
	raw, err := relay.PostWebhook(ctx, {{ quote .Provider }}, body)
{{- end }}
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(string(raw)), nil
}
{{ end }}`))

func writeGenFile(path string, tmpl *template.Template, data any) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		fmt.Fprintf(os.Stderr, "executing template for %s: %v\n", path, err)
		os.Exit(1)
	}

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		// Write unformatted for debugging
		_ = os.WriteFile(path+".raw", buf.Bytes(), 0o600)
		fmt.Fprintf(os.Stderr, "formatting %s: %v (raw output written to %s.raw)\n", path, err, path)
		os.Exit(1)
	}

	if err := os.WriteFile(path, formatted, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", path, err)
		os.Exit(1)
	}
}

// ---------- naming helpers ----------

func toSnakeCase(s string) string {
	// Convert camelCase to snake_case
	var result []rune
	for i, r := range s {
		if unicode.IsUpper(r) && i > 0 {
			result = append(result, '_')
		}
		result = append(result, unicode.ToLower(r))
	}
	return string(result)
}

// goAcronyms maps lowercase tokens to their Go-conventional uppercase form.
var goAcronyms = map[string]string{
	"ttl": "TTL", "id": "ID", "url": "URL", "uri": "URI",
	"http": "HTTP", "https": "HTTPS", "api": "API", "ip": "IP",
	"json": "JSON", "xml": "XML", "html": "HTML", "css": "CSS",
	"sql": "SQL", "ssh": "SSH", "tcp": "TCP", "udp": "UDP",
	"tls": "TLS", "ssl": "SSL", "dns": "DNS", "rpc": "RPC",
	"cpu": "CPU", "gpu": "GPU", "uid": "UID", "uuid": "UUID",
	"db": "DB", "io": "IO", "os": "OS",
}

func toPascalCase(s string) string {
	// Convert snake_case or kebab-case to PascalCase with Go acronym handling
	parts := regexp.MustCompile(`[-_]+`).Split(s, -1)
	var result string
	for _, p := range parts {
		if p == "" {
			continue
		}
		if acronym, ok := goAcronyms[strings.ToLower(p)]; ok {
			result += acronym
		} else {
			result += strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return result
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
