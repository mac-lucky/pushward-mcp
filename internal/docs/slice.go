package docs

import (
	"regexp"
	"strings"
)

var (
	// ATX headings only: 1-6 '#', a space, the text, an optional closing '#'
	// sequence. Setext (underline) headings are intentionally not supported -
	// the PushWard docs use ATX exclusively.
	headingRE = regexp.MustCompile(`^(#{1,6})[ \t]+(.+?)[ \t]*#*[ \t]*$`)
	// A line that opens or closes a fenced code block.
	fenceRE = regexp.MustCompile("^[ \\t]*(```|~~~)")
	// Slug helpers: keep [a-z0-9], spaces, and hyphens; collapse the rest.
	slugStripRE = regexp.MustCompile(`[^a-z0-9 -]+`)
	slugSpaceRE = regexp.MustCompile(` +`)
)

type heading struct {
	level int
	text  string
	slug  string
	start int // byte offset of the heading line within the normalized doc
}

// Section returns the slice of doc beginning at the heading matching section and
// ending just before the next heading of equal-or-higher level. Matching is
// case-insensitive: first by GitHub-style anchor slug, then by substring of the
// heading text. Headings inside fenced code blocks are ignored.
//
// If section is blank, the whole doc is returned with ok=true. If no heading
// matches, ok=false and topLevel lists the document's navigable headings (the
// shallowest level that has more than one entry) so the caller can guide retry.
func Section(doc, section string) (text string, ok bool, topLevel []string) {
	doc = strings.ReplaceAll(doc, "\r\n", "\n")
	headings := parseHeadings(doc)

	// topLevel is only consumed by the caller on a no-match; compute it lazily so
	// the common match / whole-doc paths don't pay for it on every call.
	if strings.TrimSpace(section) == "" {
		return doc, true, navigableHeadings(headings)
	}

	idx := matchHeading(headings, section)
	if idx < 0 {
		return "", false, navigableHeadings(headings)
	}

	start := headings[idx].start
	end := len(doc)
	for j := idx + 1; j < len(headings); j++ {
		if headings[j].level <= headings[idx].level {
			end = headings[j].start
			break
		}
	}
	return strings.TrimRight(doc[start:end], "\n") + "\n", true, topLevel
}

// parseHeadings scans doc once, recording every ATX heading found outside fenced
// code blocks. Offsets index into the (already CRLF-normalized) doc.
func parseHeadings(doc string) []heading {
	var hs []heading
	inCode := false
	offset := 0
	for _, line := range strings.SplitAfter(doc, "\n") {
		content := strings.TrimRight(line, "\n")
		switch {
		case fenceRE.MatchString(content):
			inCode = !inCode
		case !inCode:
			if m := headingRE.FindStringSubmatch(content); m != nil {
				text := strings.TrimSpace(m[2])
				hs = append(hs, heading{
					level: len(m[1]),
					text:  text,
					slug:  slugify(text),
					start: offset,
				})
			}
		}
		offset += len(line)
	}
	return hs
}

// matchHeading returns the index of the first heading matching section (exact
// slug first, then case-insensitive substring of the text), or -1.
func matchHeading(headings []heading, section string) int {
	wantSlug := slugify(section)
	if wantSlug != "" {
		for i, h := range headings {
			if h.slug == wantSlug {
				return i
			}
		}
	}
	lower := strings.ToLower(strings.TrimSpace(section))
	for i, h := range headings {
		if strings.Contains(strings.ToLower(h.text), lower) {
			return i
		}
	}
	return -1
}

// navigableHeadings returns the heading texts at the shallowest level that has
// more than one entry, falling back to the shallowest level present. This gives
// a useful "table of contents" for both the single-H1 index and the many-H1
// full bundle.
func navigableHeadings(headings []heading) []string {
	if len(headings) == 0 {
		return nil
	}
	counts := map[int]int{}
	minLevel := 7
	for _, h := range headings {
		counts[h.level]++
		if h.level < minLevel {
			minLevel = h.level
		}
	}
	chosen := minLevel
	for lvl := minLevel; lvl <= 6; lvl++ {
		if counts[lvl] > 1 {
			chosen = lvl
			break
		}
	}
	var out []string
	for _, h := range headings {
		if h.level == chosen {
			out = append(out, h.text)
		}
	}
	return out
}

// slugify produces a GitHub-anchor-style slug. It is used symmetrically on both
// the requested section name and each heading, so the two only need to agree
// with each other.
func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "_", "-")
	s = slugStripRE.ReplaceAllString(s, "")
	s = strings.TrimSpace(s)
	s = slugSpaceRE.ReplaceAllString(s, "-")
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	return strings.Trim(s, "-")
}
