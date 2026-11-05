package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func decodeSpec(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// specRefs returns every "#/components/..." string under v: $ref values and
// discriminator mappings.
func specRefs(v any, out map[string]bool) {
	switch x := v.(type) {
	case string:
		if strings.HasPrefix(x, "#/components/") {
			out[x] = true
		}
	case map[string]any:
		for _, c := range x {
			specRefs(c, out)
		}
	case []any:
		for _, c := range x {
			specRefs(c, out)
		}
	}
}

func TestTrimDocsSpec_CommittedSpec(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "openapi.yaml"))
	if err != nil {
		t.Skipf("openapi.yaml not found: %v", err)
	}
	out, err := trimDocsSpec(data)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("trimmed %d -> %d bytes", len(data), len(out))
	in, got := decodeSpec(t, data), decodeSpec(t, out)
	inPaths, gotPaths := asMap(in["paths"]), asMap(got["paths"])

	dropped := 0
	for path, item := range inPaths {
		for method, op := range asMap(item) {
			id, _ := asMap(op)["operationId"].(string)
			gotOp, kept := asMap(gotPaths[path])[method]
			if isOrgPath(path) || personalSettingsOps[id] {
				dropped++
				if kept {
					t.Errorf("%s %s (%s) is still in the docs spec", method, path, id)
				}
				continue
			}
			if !reflect.DeepEqual(op, gotOp) {
				t.Errorf("%s %s (%s) changed or went missing", method, path, id)
			}
		}
	}
	// Otherwise the checks above pass on a spec that never had these groups.
	if dropped == 0 {
		t.Fatal("openapi.yaml has no org or personal settings operations")
	}
	for path, item := range gotPaths {
		if isOrgPath(path) {
			t.Errorf("%s is still in the docs spec", path)
		}
		if len(asMap(item)) == 0 {
			t.Errorf("%s is left without operations", path)
		}
	}

	for k, v := range in {
		if k != "paths" && k != "components" && !reflect.DeepEqual(v, got[k]) {
			t.Errorf("top-level %s changed", k)
		}
	}
	inComponents, gotComponents := asMap(in["components"]), asMap(got["components"])
	for k, v := range inComponents {
		if !refOnly[k] && !reflect.DeepEqual(v, gotComponents[k]) {
			t.Errorf("components.%s changed", k)
		}
	}
	for section := range refOnly {
		for name, e := range asMap(gotComponents[section]) {
			if !reflect.DeepEqual(e, asMap(inComponents[section])[name]) {
				t.Errorf("components.%s.%s changed", section, name)
			}
		}
	}

	// Every reference resolves.
	refs := map[string]bool{}
	specRefs(got, refs)
	for ref := range refs {
		var node any = got
		for _, seg := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			node = asMap(node)[seg]
		}
		if node == nil {
			t.Errorf("%s does not resolve", ref)
		}
	}

	// Every schema, response, parameter and request body left is reached from
	// the rest of the spec.
	reached := map[string]bool{}
	queue := []any{}
	for k, v := range got {
		if k != "components" {
			queue = append(queue, v)
		}
	}
	for k, v := range gotComponents {
		if !refOnly[k] {
			queue = append(queue, v)
		}
	}
	for len(queue) > 0 {
		next := map[string]bool{}
		specRefs(queue, next)
		queue = queue[:0]
		for ref := range next {
			section, rest, _ := strings.Cut(strings.TrimPrefix(ref, "#/components/"), "/")
			name, _, _ := strings.Cut(rest, "/")
			if key := section + "/" + name; refOnly[section] && !reached[key] {
				reached[key] = true
				queue = append(queue, asMap(gotComponents[section])[name])
			}
		}
	}
	for section := range refOnly {
		for name := range asMap(gotComponents[section]) {
			if !reached[section+"/"+name] {
				t.Errorf("components.%s.%s is left with nothing reaching it", section, name)
			}
		}
	}

	// Trimming twice changes nothing.
	again, err := trimDocsSpec(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, out) {
		t.Error("trimming the trimmed spec changed it")
	}
}

// The asset get_pushward_docs serves is the trimmed spec, so a spec copied
// into it by hand fails here until the generator has run.
func TestTrimDocsSpec_CommittedAsset(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "internal", "docs", "assets", "api-openapi.yaml"))
	if err != nil {
		t.Skipf("api-openapi.yaml not found: %v", err)
	}
	out, err := trimDocsSpec(data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, data) {
		t.Errorf("internal/docs/assets/api-openapi.yaml is not trimmed (%d -> %d bytes): run PUSHWARD_USE_LOCAL_SPEC=1 go run ./cmd/generate", len(data), len(out))
	}
	for path, item := range asMap(decodeSpec(t, data)["paths"]) {
		if isOrgPath(path) {
			t.Errorf("%s is in the docs asset", path)
		}
		for _, op := range asMap(item) {
			if id, _ := asMap(op)["operationId"].(string); slices.Contains(wantPersonalSettings, id) {
				t.Errorf("%s is in the docs asset", id)
			}
		}
	}
}

const syntheticSpec = `openapi: 3.1.0
paths:
  /orgs/{id}:
    get:
      operationId: getOrg
      parameters:
        - $ref: "#/components/parameters/OrgID"
      responses:
        "200":
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/Org"
        "409":
          $ref: "#/components/responses/OrgConflict"
  /widgets:
    get:
      operationId: listWidgets
      responses:
        "200":
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/Page"
        "404":
          $ref: "#/components/responses/NotFound"
  /me:
    get:
      operationId: getMe
      responses:
        "200":
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/Me"
    patch:
      operationId: updateMe
      requestBody:
        content:
          application/json:
            schema:
              $ref: "#/components/schemas/MePatch"
  /devices:
    get:
      operationId: listDevices
      responses:
        "200":
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/Device"
components:
  schemas:
    Org:
      properties:
        owner:
          $ref: "#/components/schemas/Shared"
        extra:
          $ref: "#/components/schemas/OrgOnly"
    OrgOnly:
      type: object
    Shared:
      discriminator:
        propertyName: kind
        mapping:
          a: "#/components/schemas/Variant"
    Variant:
      type: object
    Page:
      items:
        $ref: "#/components/schemas/Shared"
    Me:
      type: object
    MePatch:
      type: object
    Device:
      properties:
        nested:
          $ref: "#/components/schemas/DeviceNested"
    DeviceNested:
      type: object
    Problem:
      type: object
    OrgConflictBody:
      type: object
  responses:
    NotFound:
      content:
        application/json:
          schema:
            $ref: "#/components/schemas/Problem"
    OrgConflict:
      content:
        application/json:
          schema:
            $ref: "#/components/schemas/OrgConflictBody"
  parameters:
    OrgID:
      name: id
      in: path
      schema:
        $ref: "#/components/schemas/OrgOnly"
  securitySchemes:
    bearer:
      type: http
`

// A schema an org operation shares with a kept one stays, and so does what a
// discriminator mapping names; what only the dropped operations reached goes,
// through a shared response or parameter too, and so does a section left empty.
func TestTrimDocsSpec_Synthetic(t *testing.T) {
	out, err := trimDocsSpec([]byte(syntheticSpec))
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	keys := func(m *yaml.Node) []string {
		var ks []string
		for i := 0; m != nil && i+1 < len(m.Content); i += 2 {
			ks = append(ks, m.Content[i].Value)
		}
		return ks
	}
	root := doc.Content[0]
	paths := mapValue(root, "paths")
	for _, tc := range []struct {
		name string
		got  []string
		want []string
	}{
		{"top-level keys", keys(root), []string{"openapi", "paths", "components"}},
		{"paths", keys(paths), []string{"/widgets", "/me"}},
		{"/me operations", keys(mapValue(paths, "/me")), []string{"get"}},
		{"schemas", keys(mapValue(mapValue(root, "components"), "schemas")), []string{"Shared", "Variant", "Page", "Me", "Problem"}},
		{"responses", keys(mapValue(mapValue(root, "components"), "responses")), []string{"NotFound"}},
		{"components", keys(mapValue(root, "components")), []string{"schemas", "responses", "securitySchemes"}},
	} {
		if !slices.Equal(tc.got, tc.want) {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}
