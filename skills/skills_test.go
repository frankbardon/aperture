package skills

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestUpdateDemandDocPresent enforces that the Update-Demand seed doc exists and
// carries frontmatter. This is the CI gate that makes the house rule
// self-protecting: deleting the rule fails the build.
func TestUpdateDemandDocPresent(t *testing.T) {
	body, ok := Get("update-demand")
	if !ok {
		t.Fatal("skills/update-demand.md is missing; the Update-Demand rule must be documented")
	}
	if _, ok := parseMetadata(body); !ok {
		t.Fatal("skills/update-demand.md has no YAML frontmatter")
	}
}

// TestEverySkillHasFrontmatter enforces that every embedded skills/*.md file
// parses to a non-empty name + description, so no surface doc rots into an
// untitled stub. As real surfaces land, per-surface coverage gates join this
// one (see update-demand.md).
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
		t.Fatal("no skills/*.md files embedded; expected at least update-demand.md")
	}
}
