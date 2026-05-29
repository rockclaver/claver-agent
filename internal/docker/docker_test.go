package docker

import (
	"context"
	"errors"
	"testing"
)

// fakeClient is a hand-rolled Client used by every Manager.Status test below.
// Each case exercises a distinct unavailable reason so the AC mapping is
// covered end-to-end.
type fakeClient struct {
	info VersionInfo
	err  error
}

func (f fakeClient) Version(ctx context.Context) (VersionInfo, error) {
	return f.info, f.err
}

func newManager(t *testing.T, c Client) *Manager {
	t.Helper()
	m, err := New(Config{Client: c})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	return m
}

// AC: docker.status returns availability + version fields when reachable.
func TestStatusReachable(t *testing.T) {
	m := newManager(t, fakeClient{info: VersionInfo{Version: "26.1.4", APIVersion: "1.45"}})
	st := m.Status(context.Background())
	if !st.Available {
		t.Fatalf("expected available, got %+v", st)
	}
	if st.Version != "26.1.4" || st.APIVersion != "1.45" {
		t.Errorf("version fields = %+v", st)
	}
	if st.UnavailableReason != "" {
		t.Errorf("reachable status must not carry an unavailable reason, got %q", st.UnavailableReason)
	}
}

// AC: missing Docker is distinguishable from a down daemon.
func TestStatusMissingDocker(t *testing.T) {
	m := newManager(t, fakeClient{err: errors.New("wrap: " + ErrNotInstalled.Error())})
	st := m.Status(context.Background())
	if st.Available {
		t.Fatal("expected unavailable")
	}
	// errors.Is requires the sentinel be wrapped, not just string-included.
	m = newManager(t, fakeClient{err: wrap(ErrNotInstalled, "socket missing")})
	st = m.Status(context.Background())
	if st.UnavailableReason != ReasonNotInstalled {
		t.Errorf("reason = %q want %q", st.UnavailableReason, ReasonNotInstalled)
	}
	if st.UnavailableMessage == "" {
		t.Error("expected human-readable message on unavailable status")
	}
}

// AC: daemon-unavailable state is distinct from permission denied.
func TestStatusDaemonDown(t *testing.T) {
	m := newManager(t, fakeClient{err: wrap(ErrDaemonDown, "connection refused")})
	st := m.Status(context.Background())
	if st.Available {
		t.Fatal("expected unavailable")
	}
	if st.UnavailableReason != ReasonDaemonDown {
		t.Errorf("reason = %q want %q", st.UnavailableReason, ReasonDaemonDown)
	}
}

// AC: socket permission denied is its own state.
func TestStatusPermissionDenied(t *testing.T) {
	m := newManager(t, fakeClient{err: wrap(ErrPermissionDenied, "EACCES")})
	st := m.Status(context.Background())
	if st.Available {
		t.Fatal("expected unavailable")
	}
	if st.UnavailableReason != ReasonPermissionDenied {
		t.Errorf("reason = %q want %q", st.UnavailableReason, ReasonPermissionDenied)
	}
}

// Anything we don't recognize collapses into ReasonUnknown rather than being
// silently dropped or surfaced as available.
func TestStatusUnknownError(t *testing.T) {
	m := newManager(t, fakeClient{err: errors.New("totally novel failure")})
	st := m.Status(context.Background())
	if st.Available {
		t.Fatal("expected unavailable")
	}
	if st.UnavailableReason != ReasonUnknown {
		t.Errorf("reason = %q want %q", st.UnavailableReason, ReasonUnknown)
	}
}

func TestNewRejectsNilClient(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected error when Client is nil")
	}
}

// wrap returns an error chain whose root is sentinel, so errors.Is works.
func wrap(sentinel error, msg string) error {
	return errWithCause{msg: msg, cause: sentinel}
}

type errWithCause struct {
	msg   string
	cause error
}

func (e errWithCause) Error() string { return e.msg + ": " + e.cause.Error() }
func (e errWithCause) Unwrap() error { return e.cause }
