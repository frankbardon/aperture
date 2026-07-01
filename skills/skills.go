// Package skills provides the embedded skill pack for Aperture's LLM-driven
// surfaces (CLI, MCP). Each feature ships a markdown skill under skills/, kept
// in sync with the code by the Update-Demand rule (see update-demand.md) and
// enforced by the gates in skills_test.go.
package skills

import (
	"embed"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed *.md
var content embed.FS

// Metadata describes a bundled skill, parsed from its YAML frontmatter.
type Metadata struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	AppliesTo   []string `json:"applies_to,omitempty"`
}

// List walks the embedded content for *.md files, parses their frontmatter, and
// returns the resulting Metadata slice sorted by Name. Files without
// frontmatter are skipped; the coverage gate in skills_test.go is the place
// that crashes on a malformed skill, not the loader.
func List() []Metadata {
	entries, err := fs.ReadDir(content, ".")
	if err != nil {
		panic("skills: cannot read embedded content: " + err.Error())
	}
	out := make([]Metadata, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		data, err := fs.ReadFile(content, entry.Name())
		if err != nil {
			continue
		}
		md, ok := parseMetadata(string(data))
		if !ok {
			continue
		}
		if md.Name == "" {
			md.Name = strings.TrimSuffix(entry.Name(), ".md")
		}
		out = append(out, md)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns the markdown content for the named skill (without the .md
// extension), and true when found.
func Get(name string) (string, bool) {
	data, err := fs.ReadFile(content, name+".md")
	if err != nil {
		return "", false
	}
	return string(data), true
}

// Names returns the sorted list of skill names.
func Names() []string {
	items := List()
	out := make([]string, len(items))
	for i, m := range items {
		out[i] = m.Name
	}
	return out
}

// parseFrontmatter extracts the simple `key: value` pairs from a leading
// ---\n...\n--- YAML block. Returns an empty map when no block is present.
func parseFrontmatter(md string) map[string]string {
	result := make(map[string]string)
	if !strings.HasPrefix(md, "---\n") {
		return result
	}
	end := strings.Index(md[4:], "\n---")
	if end < 0 {
		return result
	}
	for _, line := range strings.Split(md[4:4+end], "\n") {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			result[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	return result
}

func parseMetadata(md string) (Metadata, bool) {
	fm := parseFrontmatter(md)
	if len(fm) == 0 {
		return Metadata{}, false
	}
	return Metadata{
		Name:        fm["name"],
		Description: fm["description"],
		AppliesTo:   parseList(fm["applies_to"]),
	}, true
}

// parseList parses a `[a, b]` or `a, b` frontmatter list into a slice.
func parseList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
		raw = raw[1 : len(raw)-1]
	}
	var out []string
	for _, p := range strings.Split(raw, ",") {
		p = strings.Trim(strings.TrimSpace(p), `"'`)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
