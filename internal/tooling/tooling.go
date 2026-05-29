// Package tooling manages on-VPS install of the agent CLIs (claude, codex)
// that tmux sessions exec. The agent process runs unprivileged (NoNewPrivileges,
// ProtectSystem=strict), so installs go to a per-user prefix that lives under
// the agent's data directory and is symlinked into a stable BinDir that
// TmuxRuntime prepends to PATH.
package tooling

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Kind identifies one of the tools the agent can install.
type Kind string

const (
	KindClaude Kind = "claude"
	KindCodex  Kind = "codex"
)

// Validate returns nil iff k is a supported kind.
func (k Kind) Validate() error {
	switch k {
	case KindClaude, KindCodex:
		return nil
	}
	return fmt.Errorf("tooling: unsupported kind %q", string(k))
}

// Status reports the on-disk state of a tool.
type Status struct {
	Kind      Kind   `json:"kind"`
	Installed bool   `json:"installed"`
	Path      string `json:"path,omitempty"`
	Version   string `json:"version,omitempty"`
}

// Stream tags a progress line from a running install with its origin.
type Stream string

const (
	StreamStdout Stream = "stdout"
	StreamStderr Stream = "stderr"
	StreamSystem Stream = "system" // agent-generated lines (e.g. step headers)
)

// Line is one progress line streamed from an install.
type Line struct {
	Stream Stream
	Text   string
}

// Config configures the manager.
type Config struct {
	// BinDir is the stable directory the agent prepends to PATH when
	// launching tmux. Symlinks for installed CLIs live here.
	BinDir string
	// NpmPrefix is passed as NPM_CONFIG_PREFIX so `npm install -g` lands
	// under a writable per-user tree instead of /usr/local.
	NpmPrefix string
	// NpmCache is passed as NPM_CONFIG_CACHE (default: <NpmPrefix>/.npm-cache).
	NpmCache string

	// ClaudePackage / CodexPackage are the npm package specs installed for
	// each kind. Defaults: "@anthropic-ai/claude-code", "@openai/codex".
	ClaudePackage string
	CodexPackage  string

	// NpmBin overrides the npm executable used (default: "npm").
	NpmBin string
}

// Manager handles tool detection and install with per-kind single-flight.
type Manager struct {
	cfg Config

	mu      sync.Mutex
	running map[Kind]bool
}

// New constructs a Manager. It ensures BinDir and NpmPrefix exist.
func New(cfg Config) (*Manager, error) {
	if cfg.BinDir == "" {
		return nil, errors.New("tooling: BinDir is required")
	}
	if cfg.NpmPrefix == "" {
		return nil, errors.New("tooling: NpmPrefix is required")
	}
	if cfg.NpmCache == "" {
		cfg.NpmCache = filepath.Join(cfg.NpmPrefix, ".npm-cache")
	}
	if cfg.ClaudePackage == "" {
		cfg.ClaudePackage = "@anthropic-ai/claude-code"
	}
	if cfg.CodexPackage == "" {
		cfg.CodexPackage = "@openai/codex"
	}
	if cfg.NpmBin == "" {
		cfg.NpmBin = "npm"
	}
	for _, d := range []string{cfg.BinDir, cfg.NpmPrefix, filepath.Join(cfg.NpmPrefix, "bin"), cfg.NpmCache} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("tooling: mkdir %s: %w", d, err)
		}
	}
	return &Manager{cfg: cfg, running: make(map[Kind]bool)}, nil
}

// BinDir returns the directory callers should prepend to PATH.
func (m *Manager) BinDir() string { return m.cfg.BinDir }

// Check resolves the binary for kind and probes --version. It never fails
// on a missing binary — that's just Installed=false.
func (m *Manager) Check(ctx context.Context, kind Kind) (Status, error) {
	if err := kind.Validate(); err != nil {
		return Status{}, err
	}
	st := Status{Kind: kind}
	path := m.resolve(kind)
	if path == "" {
		return st, nil
	}
	st.Installed = true
	st.Path = path
	if v, ok := probeVersion(ctx, path); ok {
		st.Version = v
	}
	return st, nil
}

// resolve looks for the tool under BinDir first, then on PATH. Returns
// "" if not found.
func (m *Manager) resolve(kind Kind) string {
	name := string(kind)
	candidate := filepath.Join(m.cfg.BinDir, name)
	if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
		return candidate
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return ""
}

func probeVersion(ctx context.Context, path string) (string, bool) {
	cmd := exec.CommandContext(ctx, path, "--version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", false
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if line == "" {
		return "", false
	}
	return line, true
}

// ErrAlreadyRunning is returned when an install for the same kind is in
// progress on another goroutine.
var ErrAlreadyRunning = errors.New("tooling: install already running for this kind")

// Install runs the install for kind, streaming each line to onLine. It is
// safe to call onLine from concurrent goroutines (the manager serializes
// calls per install). Returns the final Status after the install completes.
func (m *Manager) Install(ctx context.Context, kind Kind, onLine func(Line)) (Status, error) {
	if err := kind.Validate(); err != nil {
		return Status{}, err
	}
	if !m.acquire(kind) {
		return Status{}, ErrAlreadyRunning
	}
	defer m.release(kind)

	pkg := m.packageFor(kind)
	emit := func(stream Stream, text string) {
		if onLine != nil {
			onLine(Line{Stream: stream, Text: text})
		}
	}
	emit(StreamSystem, fmt.Sprintf("Installing %s as %s", pkg, string(kind)))
	emit(StreamSystem, fmt.Sprintf("Prefix: %s", m.cfg.NpmPrefix))

	if _, err := exec.LookPath(m.cfg.NpmBin); err != nil {
		emit(StreamStderr, fmt.Sprintf("%s not found on PATH", m.cfg.NpmBin))
		return Status{Kind: kind}, fmt.Errorf("tooling: %s not found", m.cfg.NpmBin)
	}

	// `npm install -g <pkg>` with NPM_CONFIG_PREFIX targets our per-user
	// tree. --no-fund/--no-audit cut noise but don't change correctness.
	cmd := exec.CommandContext(ctx, m.cfg.NpmBin, "install", "-g", "--no-fund", "--no-audit", pkg)
	cmd.Env = append(os.Environ(),
		"NPM_CONFIG_PREFIX="+m.cfg.NpmPrefix,
		"NPM_CONFIG_CACHE="+m.cfg.NpmCache,
	)
	if err := runStreaming(cmd, emit); err != nil {
		emit(StreamSystem, "install failed: "+err.Error())
		return Status{Kind: kind}, err
	}

	if err := m.linkBin(kind); err != nil {
		emit(StreamSystem, "link failed: "+err.Error())
		return Status{Kind: kind}, err
	}
	emit(StreamSystem, "linked into "+m.cfg.BinDir)

	final, _ := m.Check(ctx, kind)
	if final.Installed {
		emit(StreamSystem, "version: "+final.Version)
	}
	return final, nil
}

func (m *Manager) packageFor(kind Kind) string {
	switch kind {
	case KindClaude:
		return m.cfg.ClaudePackage
	case KindCodex:
		return m.cfg.CodexPackage
	}
	return ""
}

// linkBin symlinks NpmPrefix/bin/<name> into BinDir/<name>, replacing any
// pre-existing symlink so re-installs pick up the new version.
func (m *Manager) linkBin(kind Kind) error {
	name := string(kind)
	src := filepath.Join(m.cfg.NpmPrefix, "bin", name)
	dst := filepath.Join(m.cfg.BinDir, name)
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("expected %s after install: %w", src, err)
	}
	if _, err := os.Lstat(dst); err == nil {
		_ = os.Remove(dst)
	}
	return os.Symlink(src, dst)
}

func (m *Manager) acquire(kind Kind) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running[kind] {
		return false
	}
	m.running[kind] = true
	return true
}

func (m *Manager) release(kind Kind) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.running, kind)
}

// runStreaming runs cmd and emits each stdout/stderr line to onLine as it
// arrives. Blocks until cmd exits.
func runStreaming(cmd *exec.Cmd, emit func(Stream, string)) error {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go pumpLines(&wg, stdout, StreamStdout, emit)
	go pumpLines(&wg, stderr, StreamStderr, emit)
	wg.Wait()
	return cmd.Wait()
}

func pumpLines(wg *sync.WaitGroup, r io.Reader, stream Stream, emit func(Stream, string)) {
	defer wg.Done()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		emit(stream, sc.Text())
	}
}
