package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// httpMethods are the path item keys that hold an operation.
var httpMethods = map[string]bool{
	"get": true, "put": true, "post": true, "delete": true,
	"options": true, "head": true, "patch": true, "trace": true,
}

// trimDocsSpecFile rewrites the embedded API spec in place through
// trimDocsSpec. get_pushward_docs returns the file whole, so every operation
// an agent has no tool for is context it pays for.
func trimDocsSpecFile(path string) {
	data, err := os.ReadFile(path) // #nosec G304 -- build-time constant path
	if err != nil {
		fmt.Fprintf(os.Stderr, "trim %s: %v\n", path, err)
		os.Exit(1)
	}
	trimmed, err := trimDocsSpec(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "trim %s: %v\n", path, err)
		os.Exit(1)
	}
	if bytes.Equal(trimmed, data) {
		return
	}
	// #nosec G703 -- path is a build-time constant joined to the repo root, not runtime input.
	if err := os.WriteFile(path, trimmed, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", path, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "trimmed %s (%d -> %d bytes)\n", path, len(data), len(trimmed))
}

// trimDocsSpec returns the OpenAPI YAML without the operations notForAgents
// matches, without the paths left with no operation, and without the
// component schemas nothing else reaches any more. The rest keeps its order.
func trimDocsSpec(data []byte) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("spec is not a YAML mapping")
	}
	root := doc.Content[0]
	if paths := mapValue(root, "paths"); paths != nil {
		paths.Content = keepPairs(paths.Content, func(path string, item *yaml.Node) bool {
			before := countOperations(item)
			item.Content = keepPairs(item.Content, func(method string, op *yaml.Node) bool {
				if !httpMethods[method] {
					return true
				}
				id := ""
				if n := mapValue(op, "operationId"); n != nil {
					id = n.Value
				}
				return !notForAgents(path, id)
			})
			// A path goes only with its last operation.
			after := countOperations(item)
			return after > 0 || after == before
		})
	}
	pruneSchemas(root)

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// pruneSchemas drops the component schemas that no reference reaches, starting
// from everything outside components.schemas.
func pruneSchemas(root *yaml.Node) {
	components := mapValue(root, "components")
	schemas := mapValue(components, "schemas")
	if schemas == nil {
		return
	}
	byName := map[string]*yaml.Node{}
	for i := 0; i+1 < len(schemas.Content); i += 2 {
		byName[schemas.Content[i].Value] = schemas.Content[i+1]
	}
	reached := map[string]bool{}
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Kind == yaml.ScalarNode {
			// $ref values and discriminator mappings both name a schema this way.
			if name, ok := strings.CutPrefix(n.Value, "#/components/schemas/"); ok {
				name, _, _ = strings.Cut(name, "/")
				if s := byName[name]; s != nil && !reached[name] {
					reached[name] = true
					walk(s)
				}
			}
			return
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "components" {
			walk(root.Content[i+1])
		}
	}
	for i := 0; i+1 < len(components.Content); i += 2 {
		if components.Content[i].Value != "schemas" {
			walk(components.Content[i+1])
		}
	}
	schemas.Content = keepPairs(schemas.Content, func(name string, _ *yaml.Node) bool { return reached[name] })
}

// mapValue returns the value under key in a mapping node, or nil.
func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// keepPairs filters a mapping node's key/value pairs, keeping their order.
func keepPairs(content []*yaml.Node, keep func(key string, value *yaml.Node) bool) []*yaml.Node {
	out := content[:0]
	for i := 0; i+1 < len(content); i += 2 {
		if keep(content[i].Value, content[i+1]) {
			out = append(out, content[i], content[i+1])
		}
	}
	return out
}

func countOperations(item *yaml.Node) int {
	n := 0
	for i := 0; i+1 < len(item.Content); i += 2 {
		if httpMethods[item.Content[i].Value] {
			n++
		}
	}
	return n
}
