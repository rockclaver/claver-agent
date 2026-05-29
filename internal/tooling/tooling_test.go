package tooling

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	m, err := New(Config{
		BinDir:    filepath.Join(dir, "bin"),
		NpmPrefix: filepath.Join(dir, "npm"),
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return m
}

func TestKindValidate(t *testing.T) {
	if err := KindClaude.Validate(); err != nil {
		t.Errorf("claude should validate: %v", err)
	}
	if err := KindCodex.Validate(); err != nil {
		t.Errorf("codex should validate: %v", err)
	}
	if err := Kind("nope").Validate(); err == nil {
		t.Error("expected error for unknown kind")
	}
}

func TestCheckMissing(t *testing.T) {
	m := newTestManager(t)
	// Empty BinDir; PATH lookup will also miss (claude/codex unlikely on CI).
	st, err := m.Check(context.Background(), KindClaude)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if st.Installed && st.Path == "" {
		t.Errorf("Installed=true with empty path is invalid")
	}
}

func TestCheckResolvesFromBinDir(t *testing.T) {
	m := newTestManager(t)
	// Drop a stub executable into BinDir/claude that prints a version line.
	stub := filepath.Join(m.cfg.BinDir, "claude")
	script := "#!/bin/sh\necho claude-stub 9.9.9\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	st, err := m.Check(context.Background(), KindClaude)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !st.Installed {
		t.Fatalf("expected installed, got %+v", st)
	}
	if st.Path != stub {
		t.Errorf("path = %q want %q", st.Path, stub)
	}
	if st.Version != "claude-stub 9.9.9" {
		t.Errorf("version = %q", st.Version)
	}
}

func TestInstallSingleFlight(t *testing.T) {
	m := newTestManager(t)
	// Grab the lock manually and verify a parallel Install fails fast with
	// ErrAlreadyRunning, without ever shelling out to npm.
	if !m.acquire(KindClaude) {
		t.Fatal("acquire failed")
	}
	defer m.release(KindClaude)

	var wg sync.WaitGroup
	wg.Add(1)
	var gotErr error
	go func() {
		defer wg.Done()
		_, gotErr = m.Install(context.Background(), KindClaude, nil)
	}()
	wg.Wait()
	if !errors.Is(gotErr, ErrAlreadyRunning) {
		t.Errorf("err = %v, want ErrAlreadyRunning", gotErr)
	}
}

func TestInstallRejectsBadKind(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.Install(context.Background(), Kind("nope"), nil); err == nil {
		t.Error("expected error for bad kind")
	}
}
