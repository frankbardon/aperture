package skills

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestMCPSurfaceDocPresent enforces that the surface doc exists and carries
// frontmatter — the Update-Demand gate that makes the rule self-protecting:
// deleting the doc fails the build.
func TestMCPSurfaceDocPresent(t *testing.T) {
	body, ok := Get("mcp-surface")
	if !ok {
		t.Fatal("mcp/skills/mcp-surface.md is missing; the MCP surface must be documented")
	}
	if _, ok := parseMetadata(body); !ok {
		t.Fatal("mcp/skills/mcp-surface.md has no YAML frontmatter")
	}
}

// TestEverySkillHasFrontmatter enforces that every embedded *.md doc parses to a
// non-empty name + description whose name matches the file stem, so no surface
// doc rots into an untitled stub. Mirrors the repo-root skills enforcing test.
func TestEverySkillHasFrontmatter(t *testing.T) {
	entries, err := fs.ReadDir(content, ".")
	if err != nil {
		t.Fatalf("read embedded skills dir: %v", err)
	}
	seen := 0
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".md" {
			continue
		}
		seen++
		data, err := fs.ReadFile(content, e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		md, ok := parseMetadata(string(data))
		if !ok {
			t.Errorf("%s: missing YAML frontmatter", e.Name())
			continue
		}
		stem := strings.TrimSuffix(e.Name(), ".md")
		if md.Name == "" {
			t.Errorf("%s: frontmatter missing `name`", e.Name())
		}
		if md.Name != "" && md.Name != stem {
			t.Errorf("%s: frontmatter name %q does not match file stem %q", e.Name(), md.Name, stem)
		}
		if md.Description == "" {
			t.Errorf("%s: frontmatter missing `description`", e.Name())
		}
	}
	if seen == 0 {
		t.Fatal("no mcp/skills/*.md files embedded; expected at least mcp-surface.md")
	}
}
