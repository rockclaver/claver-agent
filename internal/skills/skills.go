// Package skills enumerates the slash-command surface a mobile client can
// offer in the prompt composer: the agent's installed skills (SKILL.md
// directories on disk) plus a curated set of built-in CLI commands. It is
// read-only — invocation still happens by sending the chosen token as a normal
// prompt, so this package only has to answer "what can the user pick?".
package skills

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrBadAgent mirrors sessions.ErrBadAgent: only claude and codex are known.
var ErrBadAgent = errors.New("agent must be claude or codex")

// Item is one pickable entry. Name is the bare identifier (no leading / or $);
// Description is the human-readable summary surfaced in the dropdown.
type Item struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Catalog is the full pickable surface for one agent. Skills come from disk;
// Commands is the curated built-in list. Both are sorted by Name.
type Catalog struct {
	Agent    string `json:"agent"`
	Skills   []Item `json:"skills"`
	Commands []Item `json:"commands"`
}

// Manager resolves the on-disk skill directories under a fixed HomeDir, which
// matches the HOME the agent CLIs run with (see sessions.TmuxRuntime.HomeDir).
type Manager struct {
	HomeDir string
}

func New(homeDir string) *Manager {
	return &Manager{HomeDir: homeDir}
}

// List returns the pickable catalog for agent. A missing skills directory is
// not an error — it just yields an empty skill list — so a fresh install with
// no skills still returns the built-in commands.
func (m *Manager) List(agent string) (Catalog, error) {
	if agent != "claude" && agent != "codex" {
		return Catalog{}, ErrBadAgent
	}
	cat := Catalog{
		Agent:    agent,
		Skills:   m.scanSkills(m.skillsDir(agent)),
		Commands: builtinCommands(agent),
	}
	return cat, nil
}

func (m *Manager) skillsDir(agent string) string {
	switch agent {
	case "codex":
		return filepath.Join(m.HomeDir, ".codex", "skills")
	default:
		return filepath.Join(m.HomeDir, ".claude", "skills")
	}
}

// scanSkills reads each immediate subdirectory's SKILL.md and parses its
// frontmatter. Entries are deduplicated by name (the first wins) and sorted.
// Directory entries are read with os.ReadDir, which resolves symlinked dirs
// (the common case: ~/.claude/skills/* are symlinks into a shared store).
func (m *Manager) scanSkills(dir string) []Item {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	seen := make(map[string]struct{})
	items := make([]Item, 0, len(entries))
	for _, e := range entries {
		if !isDirEntry(dir, e) {
			continue
		}
		name, desc, ok := parseSkillFile(filepath.Join(dir, e.Name(), "SKILL.md"))
		if !ok {
			continue
		}
		if name == "" {
			name = e.Name()
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		items = append(items, Item{Name: name, Description: desc})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items
}

// isDirEntry reports whether e is a directory, following one level of symlink
// (skill entries are frequently symlinks to a shared skills store).
func isDirEntry(parent string, e os.DirEntry) bool {
	if e.IsDir() {
		return true
	}
	if e.Type()&os.ModeSymlink == 0 {
		return false
	}
	info, err := os.Stat(filepath.Join(parent, e.Name()))
	return err == nil && info.IsDir()
}

// parseSkillFile extracts name and description from a SKILL.md YAML frontmatter
// block. It is intentionally minimal — it reads the leading `---` fenced block
// and pulls the two keys it cares about — rather than pulling in a YAML
// dependency for a two-field header.
func parseSkillFile(path string) (name, desc string, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	inFront := false
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "---" {
			if !inFront {
				inFront = true
				continue
			}
			break // end of frontmatter
		}
		if !inFront {
			// No frontmatter fence at the top of the file.
			if trimmed == "" {
				continue
			}
			return "", "", false
		}
		key, val, found := strings.Cut(trimmed, ":")
		if !found {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "name":
			name = unquote(strings.TrimSpace(val))
		case "description":
			desc = unquote(strings.TrimSpace(val))
		}
	}
	if name == "" && desc == "" {
		return "", "", false
	}
	return name, desc, true
}

func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// builtinCommands returns the curated set of always-available CLI slash
// commands per agent. These are CLI features (not on-disk skills), so they are
// hardcoded; they change only with a CLI upgrade, not per install.
func builtinCommands(agent string) []Item {
	switch agent {
	case "codex":
		return []Item{
			{Name: "approvals", Description: "Choose what Codex can do without approval"},
			{Name: "clear", Description: "Start a new conversation, clearing context"},
			{Name: "compact", Description: "Summarize the conversation to free up context"},
			{Name: "diff", Description: "Show working-tree changes since the session started"},
			{Name: "init", Description: "Create or update an AGENTS.md with project guidance"},
			{Name: "mention", Description: "Reference a file in the conversation"},
			{Name: "model", Description: "Switch the model and reasoning effort"},
			{Name: "new", Description: "Begin a brand new chat session"},
			{Name: "review", Description: "Ask Codex to review the current changes"},
			{Name: "status", Description: "Show session token usage and configuration"},
		}
	default: // claude
		return []Item{
			{Name: "clear", Description: "Clear the conversation history"},
			{Name: "compact", Description: "Summarize the conversation to free up context"},
			{Name: "config", Description: "Open the configuration panel"},
			{Name: "cost", Description: "Show token usage and cost for the session"},
			{Name: "init", Description: "Generate a CLAUDE.md with codebase guidance"},
			{Name: "model", Description: "Switch the active Claude model"},
			{Name: "review", Description: "Review a pull request or the current changes"},
			{Name: "security-review", Description: "Audit the changes for security issues"},
		}
	}
}
