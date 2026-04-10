// Command generate reads both OpenAPI specs and emits typed MCP tool
// definitions into internal/tools/api_gen.go and internal/tools/relay_gen.go.
package main

import (
	"bytes"
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
	"strings"
	"text/template"
	"unicode"

	"gopkg.in/yaml.v3"
)

// ---------- minimal OpenAPI 3.1 AST ----------

type openAPISpec struct {
	Paths      map[string]pathItem            `json:"paths" yaml:"paths"`
	Components struct {
		Schemas map[string]schemaObj `json:"schemas" yaml:"schemas"`
	} `json:"components" yaml:"components"`
}

type pathItem map[string]operation // "get", "post", etc.

type operation struct {
	OperationID string      `json:"operationId" yaml:"operationId"`
	Summary     string      `json:"summary" yaml:"summary"`
	Description string      `json:"description" yaml:"description"`
	RequestBody *requestBody `json:"requestBody" yaml:"requestBody"`
	Security    []any       `json:"security" yaml:"security"`
}

type requestBody struct {
	Required bool                       `json:"required" yaml:"required"`
	Content  map[string]mediaTypeObject `json:"content" yaml:"content"`
}

type mediaTypeObject struct {
	Schema schemaObj `json:"schema" yaml:"schema"`
}

type schemaObj struct {
	Type       any              `json:"type" yaml:"type"` // string or []string
	Ref        string           `json:"$ref" yaml:"$ref"`
	Properties map[string]schemaObj `json:"properties" yaml:"properties"`
	Required   []string         `json:"required" yaml:"required"`
	Items      *schemaObj       `json:"items" yaml:"items"`
	Enum       []string         `json:"enum" yaml:"enum"`
	Format     string           `json:"format" yaml:"format"`
	Maximum    *float64         `json:"maximum" yaml:"maximum"`
	Minimum    *float64         `json:"minimum" yaml:"minimum"`
	MaxLength  *int             `json:"maxLength" yaml:"maxLength"`
	MinLength  *int             `json:"minLength" yaml:"minLength"`
	Pattern    string           `json:"pattern" yaml:"pattern"`
	ReadOnly   bool             `json:"readOnly" yaml:"readOnly"`
	Desc       string           `json:"description" yaml:"description"`
	Examples   []any            `json:"examples" yaml:"examples"`
	AdditionalProperties any    `json:"additionalProperties" yaml:"additionalProperties"`
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
	ContentJSON bool // true if body is passed as a single content_json string
	IsRelay     bool
	Provider    string // relay provider name
	PayloadJSON bool   // relay: entire body as payload_json
}

type paramDef struct {
	Name     string
	GoName   string
	MCPType  string // "String", "Number", "Boolean"
	Desc     string
	Required bool
	Enum     []string
}

// Live OpenAPI spec URLs.
const (
	apiSpecURL   = "https://api.pushward.app/openapi.json"
	relaySpecURL = "https://relay.pushward.app/openapi.json"
)

// ---------- main ----------

func main() {
	rootDir := findRootDir()
	outDir := filepath.Join(rootDir, "internal", "tools")

	// Fetch API spec from live URL, fall back to local file
	apiData := fetchOrRead(apiSpecURL, filepath.Join(rootDir, "openapi.yaml"))
	apiSpec := parseSpecJSON(apiData, "api")
	apiTools := buildAPITools(apiSpec)

	// Fetch Relay spec from live URL, fall back to local file
	relayData := fetchOrRead(relaySpecURL, filepath.Join(rootDir, "relay-openapi.json"))
	relaySpec := parseSpecJSON(relayData, "relay")
	relayTools := buildRelayTools(relaySpec)

	// Generate files
	writeGenFile(filepath.Join(outDir, "api_gen.go"), apiToolsTemplate, apiTools)
	writeGenFile(filepath.Join(outDir, "relay_gen.go"), relayToolsTemplate, relayTools)

	fmt.Printf("Generated %d API tools and %d relay tools\n", len(apiTools), len(relayTools))
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

// fetchOrRead tries to fetch data from a URL first; on failure falls back to a local file.
func fetchOrRead(url, fallbackPath string) []byte {
	resp, err := http.Get(url)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			data, err := io.ReadAll(resp.Body)
			if err == nil {
				fmt.Fprintf(os.Stderr, "fetched %s (%d bytes)\n", url, len(data))
				return data
			}
		}
	}
	fmt.Fprintf(os.Stderr, "fetch %s failed, falling back to %s\n", url, fallbackPath)
	data, err := os.ReadFile(fallbackPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reading %s: %v\n", fallbackPath, err)
		os.Exit(1)
	}
	return data
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
// in composite.go instead of being generated. See composite.go for their
// enhanced implementations.
var skipOperations = map[string]bool{
	"listActivities": true, // enhanced with state/source filtering and summary mode
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

			// Extract request body parameters
			if op.RequestBody != nil {
				if ct, ok := op.RequestBody.Content["application/json"]; ok {
					schema := resolveRef(spec, ct.Schema)
					t.HasBody = true
					t.Params = schemaToParams(spec, schema, op.RequestBody.Required)

					// If the schema has a "content" field that is a complex object,
					// use content_json approach
					if _, hasContent := schema.Properties["content"]; hasContent {
						t.ContentJSON = true
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
		prop := schema.Properties[name]
		if prop.ReadOnly {
			continue
		}

		// Skip $schema meta fields
		if name == "$schema" {
			continue
		}

		// Resolve $ref if needed
		prop = resolveRef(spec, prop)

		// Skip complex nested objects — they'll be handled as content_json
		if name == "content" && hasNestedProperties(prop) {
			continue
		}

		// Skip object-typed fields (e.g., metadata: map[string]string) — not representable as simple MCP params
		if schemaType(prop) == "object" || (prop.AdditionalProperties != nil && len(prop.Properties) == 0) {
			continue
		}

		p := paramDef{
			Name:     name,
			GoName:   toPascalCase(name),
			Desc:     prop.Desc,
			Required: requiredSet[name] && bodyRequired,
			Enum:     prop.Enum,
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
					rangeStr += fmt.Sprintf("min: %v", *prop.Minimum)
				}
				if prop.Maximum != nil {
					if rangeStr != "" {
						rangeStr += ", "
					}
					rangeStr += fmt.Sprintf("max: %v", *prop.Maximum)
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

func hasNestedProperties(s schemaObj) bool {
	return len(s.Properties) > 0 || s.Ref != ""
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
	"quote":       func(s string) string { return fmt.Sprintf("%q", s) },
	"hasEnum":     func(p paramDef) bool { return len(p.Enum) > 0 },
	"enumList":    func(p paramDef) string {
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

func registerAPITools(s *mcpserver.MCPServer, api *client.APIClient) {
{{- range . }}

	// {{ .Name }}
	s.AddTool(
		mcp.NewTool({{ quote .Name }},
			mcp.WithDescription({{ quote .Description }}),
{{- range .PathParams }}
			mcp.WithString({{ quote .Name }},
				mcp.Required(),
				mcp.Description({{ quote .Desc }}),
			),
{{- end }}
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
{{- if .ContentJSON }}
			mcp.WithString("content_json",
				mcp.Required(),
				mcp.Description("Activity content as JSON object. Fields: template (generic|countdown|steps|alert|gauge|timeline), progress (0.0-1.0), state, icon, subtitle, accent_color, background_color, text_color. Template-specific: countdown (duration, end_date, warning_threshold, completion_message), steps (current_step, total_steps, step_labels), alert (severity: critical|warning|info, fired_at), gauge (value, min_value, max_value, unit), timeline (value as {key:number}, history as {key:[{t,v}]}, scale, thresholds)."),
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
	input := client.{{ .FuncName }}Input{
{{- range .Params }}
{{- if eq .MCPType "String" }}
{{- if .Required }}
		{{ .GoName }}: func() string { v, _ := req.RequireString({{ quote .Name }}); return v }(),
{{- end }}
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
	if v, err := req.RequireFloat({{ quote .Name }}); err == nil {
		input.{{ .GoName }} = &v
	}
{{- else }}
	if v := req.GetFloat({{ quote .Name }}, math.NaN()); !math.IsNaN(v) {
		input.{{ .GoName }} = &v
	}
{{- end }}
{{- else if eq .MCPType "Boolean" }}
	input.{{ .GoName }} = req.GetBool({{ quote .Name }}, false)
{{- end }}
{{- end }}
{{- end }}
{{- if .ContentJSON }}
	contentStr, err := req.RequireString("content_json")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if !json.Valid([]byte(contentStr)) {
		return mcp.NewToolResultError("content_json is not valid JSON"), nil
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
{{- if and (not .Required) (eq .MCPType "Number") }}
	if v := req.GetFloat({{ quote .Name }}, math.NaN()); !math.IsNaN(v) {
		input.{{ .GoName }} = &v
	}
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
	if !json.Valid([]byte(payloadStr)) {
		return mcp.NewToolResultError("payload_json is not valid JSON"), nil
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
	if v := req.GetBool({{ quote .Name }}, false); v {
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
		_ = os.WriteFile(path+".raw", buf.Bytes(), 0644)
		fmt.Fprintf(os.Stderr, "formatting %s: %v (raw output written to %s.raw)\n", path, err, path)
		os.Exit(1)
	}

	if err := os.WriteFile(path, formatted, 0644); err != nil {
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
