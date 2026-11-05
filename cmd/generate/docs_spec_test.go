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
		if k != "schemas" && !reflect.DeepEqual(v, gotComponents[k]) {
			t.Errorf("components.%s changed", k)
		}
	}
	inSchemas, gotSchemas := asMap(inComponents["schemas"]), asMap(gotComponents["schemas"])
	for name, s := range gotSchemas {
		if !reflect.DeepEqual(s, inSchemas[name]) {
			t.Errorf("schema %s changed", name)
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

	// Every schema left is reached from outside components.schemas.
	reached := map[string]bool{}
	queue := []any{}
	for k, v := range got {
		if k != "components" {
			queue = append(queue, v)
		}
	}
	for k, v := range gotComponents {
		if k != "schemas" {
			queue = append(queue, v)
		}
	}
	for len(queue) > 0 {
		next := map[string]bool{}
		specRefs(queue, next)
		queue = queue[:0]
		for ref := range next {
			name, ok := strings.CutPrefix(ref, "#/components/schemas/")
			name, _, _ = strings.Cut(name, "/")
			if ok && !reached[name] {
				reached[name] = true
				queue = append(queue, gotSchemas[name])
			}
		}
	}
	for name := range gotSchemas {
		if !reached[name] {
			t.Errorf("schema %s is left with nothing reaching it", name)
		}
	}

	// A regeneration over the committed asset must be a no-op.
	again, err := trimDocsSpec(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, out) {
		t.Error("trimming the trimmed spec changed it")
	}
}

const syntheticSpec = `openapi: 3.1.0
paths:
  /orgs/{id}:
    get:
      operationId: getOrg
      responses:
        "200":
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/Org"
  /widgets:
    get:
      operationId: listWidgets
      responses:
        "200":
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/Page"
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
  securitySchemes:
    bearer:
      type: http
`

// A schema an org operation shares with a kept one stays, and so does what a
// discriminator mapping names; what only the dropped operations reached goes.
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
		{"schemas", keys(mapValue(mapValue(root, "components"), "schemas")), []string{"Shared", "Variant", "Page", "Me"}},
		{"components", keys(mapValue(root, "components")), []string{"schemas", "securitySchemes"}},
	} {
		if !slices.Equal(tc.got, tc.want) {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}
