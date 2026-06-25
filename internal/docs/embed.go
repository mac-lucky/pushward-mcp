// Package docs serves PushWard's reference material - the published llms.txt /
// llms-full.txt docs, the API and relay OpenAPI specs, and a curated
// best-practices guide - embedded into the binary at build time so coding
// agents can pull it into context offline. The assets are refreshed by
// cmd/generate, except best-practices.md, which is hand-authored.
package docs

import (
	"embed"
	"fmt"
)

// Explicit per-file patterns (not a bare `assets` dir) so a missing required
// asset is a compile error naming the file, and stray files can't be embedded.
//
//go:embed assets/llms.txt assets/llms-full.txt assets/api-openapi.yaml assets/relay-openapi.json assets/best-practices.md
var assetsFS embed.FS

// Kind identifies a reference document. Values mirror the get_pushward_docs
// "kind" enum.
type Kind string

const (
	KindIndex        Kind = "index"
	KindFull         Kind = "full"
	KindAPIOpenAPI   Kind = "api_openapi"
	KindRelayOpenAPI Kind = "relay_openapi"
)

var kindFiles = map[Kind]string{
	KindIndex:        "assets/llms.txt",
	KindFull:         "assets/llms-full.txt",
	KindAPIOpenAPI:   "assets/api-openapi.yaml",
	KindRelayOpenAPI: "assets/relay-openapi.json",
}

// Doc returns the full text of the reference document for kind. It returns an
// error for an unrecognized kind - the get_pushward_docs handler relies on this
// to validate input, since mcp-go does not enforce enums server-side.
func Doc(kind Kind) (string, error) {
	path, ok := kindFiles[kind]
	if !ok {
		return "", fmt.Errorf("unknown docs kind %q", kind)
	}
	b, err := assetsFS.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Kinds returns the valid Kind values as strings, for tool enums and error
// messages.
func Kinds() []string {
	return []string{
		string(KindIndex), string(KindFull),
		string(KindAPIOpenAPI), string(KindRelayOpenAPI),
	}
}

// IsMarkdown reports whether kind is a heading-structured Markdown document that
// supports section slicing (as opposed to an OpenAPI spec, which is returned
// whole).
func IsMarkdown(kind Kind) bool {
	return kind == KindIndex || kind == KindFull
}

// BestPractices returns the curated integration best-practices guide.
func BestPractices() (string, error) {
	b, err := assetsFS.ReadFile("assets/best-practices.md")
	if err != nil {
		return "", err
	}
	return string(b), nil
}
