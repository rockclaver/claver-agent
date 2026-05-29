package cliauth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/rockclaver/claver/agent/internal/github"
	"github.com/rockclaver/claver/agent/internal/store"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	vault := github.NewTokenVault(
		filepath.Join(dir, "key"),
		filepath.Join(dir, "blobs"),
	)
	m, err := New(Config{
		BinDir:  filepath.Join(dir, "bin"),
		HomeDir: dir,
		Vault:   vault,
		Store:   st,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return m
}

func TestStatusUnauthenticated(t *testing.T) {
	m := newTestManager(t)
	st, err := m.Status(context.Background(), KindClaude)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.LoggedIn {
		t.Errorf("expected LoggedIn=false, got %+v", st)
	}
	if st.Method != MethodNone {
		t.Errorf("method = %q want none", st.Method)
	}
}

func TestSetTokenRoundtrip(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.SetToken(context.Background(), KindClaude, ModeToken, "sk-ant-secret-XYZ"); err != nil {
		t.Fatalf("set_token: %v", err)
	}
	st, _ := m.Status(context.Background(), KindClaude)
	if !st.LoggedIn || st.Method != MethodToken {
		t.Errorf("post-set status = %+v", st)
	}
	secrets := m.Secrets(KindClaude)
	if got := secrets["CLAUDE_CODE_OAUTH_TOKEN"]; got != "sk-ant-secret-XYZ" {
		t.Errorf("env = %q want sk-ant-secret-XYZ", got)
	}
}

func TestSetTokenCodexAPIKey(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.SetToken(context.Background(), KindCodex, ModeAPIKey, "sk-openai-XYZ"); err != nil {
		t.Fatalf("set_token: %v", err)
	}
	secrets := m.Secrets(KindCodex)
	if got := secrets["OPENAI_API_KEY"]; got != "sk-openai-XYZ" {
		t.Errorf("env = %q want sk-openai-XYZ", got)
	}
}

func TestSetTokenRejectsBadKind(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.SetToken(context.Background(), "nope", ModeToken, "x"); !errors.Is(err, ErrBadKind) {
		t.Errorf("err = %v want ErrBadKind", err)
	}
}

func TestStartLoginUnsupportedMode(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.StartLogin(context.Background(), KindClaude, "nonsense"); !errors.Is(err, ErrBadMode) {
		t.Errorf("err = %v want ErrBadMode", err)
	}
}

func TestStartLoginSingleFlight(t *testing.T) {
	m := newTestManager(t)
	// Reserve the slot manually to simulate a running login without
	// shelling out to tmux.
	m.mu.Lock()
	m.running[KindClaude] = &Login{ID: "x", Kind: KindClaude}
	m.mu.Unlock()
	if _, err := m.StartLogin(context.Background(), KindClaude, ModeInteractive); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("err = %v want ErrAlreadyRunning", err)
	}
}

func TestScrubSecrets(t *testing.T) {
	cases := []struct{ in, want string }{
		{"prefix sk-1234567890abcdefghijklmnop end", "prefix sk-[REDACTED] end"},
		{"oauth_aaaaaaaaaaaaaaaaaaaaaaa more", "oauth_[REDACTED] more"},
		{"nothing here", "nothing here"},
	}
	for _, tc := range cases {
		if got := scrubSecrets(tc.in); got != tc.want {
			t.Errorf("scrubSecrets(%q) = %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestStripANSI(t *testing.T) {
	in := "\x1b[31mhello\x1b[0m world"
	if got := stripANSI(in); got != "hello world" {
		t.Errorf("stripANSI = %q want %q", got, "hello world")
	}
}

// Mirrors of the regexes used in driveLogin so we can sanity-check them
// without spinning up tmux. If these drift apart we'll see it in real use,
// but at least the patterns themselves are pinned by name.
var (
	urlReTest  = regexp.MustCompile(`https?://[^\s)>"']+`)
	codeReTest = regexp.MustCompile(`(?i)code[: ]+([A-Z0-9-]{6,})`)
)

func TestURLAndCodeExtraction(t *testing.T) {
	cases := []struct {
		line, url, code string
	}{
		{"open https://claude.ai/oauth/authorize?code=abc to continue", "https://claude.ai/oauth/authorize?code=abc", ""},
		{"Go to https://chatgpt.com/login and enter code: ABCD-1234", "https://chatgpt.com/login", "ABCD-1234"},
	}
	for _, tc := range cases {
		u := urlReTest.FindString(tc.line)
		if u != tc.url {
			t.Errorf("url(%q) = %q want %q", tc.line, u, tc.url)
		}
		var code string
		if m := codeReTest.FindStringSubmatch(tc.line); len(m) > 1 {
			code = m[1]
		}
		if code != tc.code {
			t.Errorf("code(%q) = %q want %q", tc.line, code, tc.code)
		}
	}
}

func TestParseAccountEmail(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "creds.json")
	if err := os.WriteFile(p, []byte(`{"oauth":{"email":"a@b.co","access_token":"t"}}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := parseAccount(KindClaude, p); got != "a@b.co" {
		t.Errorf("parseAccount = %q", got)
	}
}

func TestIsPublicURL(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"https://chatgpt.com/oauth/login?x=1", true},
		{"https://claude.ai/oauth?code=abc", true},
		{"http://localhost:1455/callback", false},
		{"http://127.0.0.1:1455/callback", false},
		{"http://192.168.1.5/", false},
		{"http://10.0.0.1/", false},
		{"http://172.16.5.5/", false},
		{"ftp://example.com/", false},
		{"https://", false},
	}
	for _, tc := range cases {
		if got := isPublicURL(tc.in); got != tc.want {
			t.Errorf("isPublicURL(%q) = %v want %v", tc.in, got, tc.want)
		}
	}
}

func TestExtractCallbackTarget(t *testing.T) {
	oauth := "https://chatgpt.com/oauth/authorize?client_id=x&redirect_uri=http%3A%2F%2Flocalhost%3A1455%2Fcallback&state=abc"
	host, port, path, ok := extractCallbackTarget(oauth)
	if !ok {
		t.Fatal("expected ok")
	}
	if host != "localhost" || port != 1455 || path != "/callback" {
		t.Errorf("got host=%q port=%d path=%q", host, port, path)
	}
}

func TestExtractCallbackTargetDefaultPort(t *testing.T) {
	oauth := "https://x/oauth?redirect_uri=https%3A%2F%2Fapi.example.com%2Fcb"
	_, port, _, ok := extractCallbackTarget(oauth)
	if !ok {
		t.Fatal("expected ok")
	}
	if port != 443 {
		t.Errorf("port = %d want 443", port)
	}
}

func TestExtractCallbackTargetMissing(t *testing.T) {
	if _, _, _, ok := extractCallbackTarget("https://nope/?x=1"); ok {
		t.Error("expected !ok when redirect_uri absent")
	}
}

func TestExtractTokenClaudeOAuth(t *testing.T) {
	body := []byte(`{"oauth":{"access_token":"abc123"}}`)
	if got := extractToken(KindClaude, body); got != "abc123" {
		t.Errorf("extractToken = %q want abc123", got)
	}
}
