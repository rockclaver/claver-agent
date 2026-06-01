package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func writeSkill(t *testing.T, dir, name, body string) {
	t.Helper()
	skillDir := filepath.Join(dir, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestListParsesFrontmatterAndSorts(t *testing.T) {
	home := t.TempDir()
	claudeSkills := filepath.Join(home, ".claude", "skills")
	writeSkill(t, claudeSkills, "tdd", "---\nname: tdd\ndescription: Test-driven development loop.\n---\n# TDD\n")
	writeSkill(t, claudeSkills, "audit", "---\nname: \"audit\"\ndescription: \"Audit the codebase.\"\n---\n")
	// A directory without SKILL.md must be skipped.
	if err := os.MkdirAll(filepath.Join(claudeSkills, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	cat, err := New(home).List("claude")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(cat.Skills) != 2 {
		t.Fatalf("want 2 skills, got %d: %+v", len(cat.Skills), cat.Skills)
	}
	if cat.Skills[0].Name != "audit" || cat.Skills[1].Name != "tdd" {
		t.Fatalf("skills not sorted by name: %+v", cat.Skills)
	}
	if cat.Skills[1].Description != "Test-driven development loop." {
		t.Fatalf("unexpected description: %q", cat.Skills[1].Description)
	}
	if cat.Skills[0].Description != "Audit the codebase." {
		t.Fatalf("quotes not stripped: %q", cat.Skills[0].Description)
	}
	if len(cat.Commands) == 0 {
		t.Fatal("expected built-in commands for claude")
	}
}

func TestNameFallsBackToDirName(t *testing.T) {
	home := t.TempDir()
	writeSkill(t, filepath.Join(home, ".codex", "skills"), "pdf",
		"---\ndescription: Work with PDFs.\n---\n")
	cat, err := New(home).List("codex")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(cat.Skills) != 1 || cat.Skills[0].Name != "pdf" {
		t.Fatalf("name should fall back to dir name: %+v", cat.Skills)
	}
}

func TestMissingSkillsDirIsNotAnError(t *testing.T) {
	cat, err := New(t.TempDir()).List("claude")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(cat.Skills) != 0 {
		t.Fatalf("want no skills, got %+v", cat.Skills)
	}
	if len(cat.Commands) == 0 {
		t.Fatal("built-in commands should still be present")
	}
}

func TestBadAgent(t *testing.T) {
	if _, err := New(t.TempDir()).List("gemini"); err != ErrBadAgent {
		t.Fatalf("want ErrBadAgent, got %v", err)
	}
}

func TestCodexUsesCodexSkillsDir(t *testing.T) {
	home := t.TempDir()
	// A claude skill must NOT leak into the codex catalog.
	writeSkill(t, filepath.Join(home, ".claude", "skills"), "tdd",
		"---\nname: tdd\ndescription: x\n---\n")
	cat, err := New(home).List("codex")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(cat.Skills) != 0 {
		t.Fatalf("codex should not see claude skills: %+v", cat.Skills)
	}
}
