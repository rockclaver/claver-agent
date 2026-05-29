// Package docker implements the Docker Manager deep module. Phase 1 exposes
// only daemon-availability detection; later phases extend the same Manager
// with container/image/volume/network reads and guarded lifecycle actions.
//
// The Manager talks to Docker exclusively through a small Client interface so
// agent tests can drive every unavailable state (missing, daemon down,
// permission denied) without a real Docker socket.
package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Unavailable reason codes returned in Status.UnavailableReason. Stable
// machine-readable strings — the UI maps these to copy.
const (
	ReasonNotInstalled     = "not_installed"
	ReasonDaemonDown       = "daemon_down"
	ReasonPermissionDenied = "permission_denied"
	ReasonUnknown          = "unknown"
)

// Typed errors a Client may return. The Manager classifies any other error
// as ReasonUnknown.
var (
	ErrNotInstalled     = errors.New("docker: not installed")
	ErrDaemonDown       = errors.New("docker: daemon unreachable")
	ErrPermissionDenied = errors.New("docker: permission denied")
)

// VersionInfo is the subset of the Docker /version response the agent needs
// for Phase 1.
type VersionInfo struct {
	Version    string
	APIVersion string
}

// Client is the agent's narrow view of the Docker Engine. Real callers get a
// HTTP-over-unix-socket client; tests pass a fake.
type Client interface {
	Version(ctx context.Context) (VersionInfo, error)
}

// Status is the typed daemon status returned by Manager.Status.
type Status struct {
	Available          bool   `json:"available"`
	Version            string `json:"version,omitempty"`
	APIVersion         string `json:"api_version,omitempty"`
	UnavailableReason  string `json:"unavailable_reason,omitempty"`
	UnavailableMessage string `json:"unavailable_message,omitempty"`
}

// Config configures the Manager.
type Config struct {
	// Client probes the Docker daemon. Required.
	Client Client
}

// Manager is the Docker deep module. Phase 1 exposes only Status; later
// phases bolt container/image/volume/network reads onto the same type.
type Manager struct {
	client Client
}

// New constructs a Manager backed by client. client must be non-nil.
func New(cfg Config) (*Manager, error) {
	if cfg.Client == nil {
		return nil, errors.New("docker: Client is required")
	}
	return &Manager{client: cfg.Client}, nil
}

// Status probes the Docker daemon and returns a typed availability snapshot.
// It never returns an error: every failure mode collapses into an unavailable
// Status with a machine-readable reason.
func (m *Manager) Status(ctx context.Context) Status {
	v, err := m.client.Version(ctx)
	if err == nil {
		return Status{
			Available:  true,
			Version:    v.Version,
			APIVersion: v.APIVersion,
		}
	}
	return Status{
		Available:          false,
		UnavailableReason:  classify(err),
		UnavailableMessage: err.Error(),
	}
}

func classify(err error) string {
	switch {
	case errors.Is(err, ErrNotInstalled):
		return ReasonNotInstalled
	case errors.Is(err, ErrPermissionDenied):
		return ReasonPermissionDenied
	case errors.Is(err, ErrDaemonDown):
		return ReasonDaemonDown
	}
	return ReasonUnknown
}

// DefaultSocketPath is the Docker Engine unix socket on Linux installs.
const DefaultSocketPath = "/var/run/docker.sock"

// SocketClient is the production Client implementation. It speaks HTTP over a
// Unix socket. Errors are translated into the package-typed sentinels so the
// Manager can classify them.
type SocketClient struct {
	socketPath string
	httpc      *http.Client
}

// NewSocketClient returns a SocketClient bound to socketPath. If socketPath is
// empty, DefaultSocketPath is used.
func NewSocketClient(socketPath string) *SocketClient {
	if socketPath == "" {
		socketPath = DefaultSocketPath
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _ string, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
		DisableKeepAlives: true,
	}
	return &SocketClient{
		socketPath: socketPath,
		httpc:      &http.Client{Transport: tr, Timeout: 5 * time.Second},
	}
}

// Version calls GET /version on the Docker socket and maps every connection
// failure mode into one of the package-typed errors.
func (c *SocketClient) Version(ctx context.Context) (VersionInfo, error) {
	// A missing socket is ambiguous: dockerd removes /var/run/docker.sock
	// when it stops, so ENOENT does not prove Docker is uninstalled. Only
	// claim "not installed" when neither the socket nor a `docker` binary
	// on PATH is present; otherwise prefer the daemon-down classification.
	if fi, err := os.Stat(c.socketPath); err != nil {
		switch {
		case errors.Is(err, os.ErrPermission):
			return VersionInfo{}, fmt.Errorf("%w: stat %s: %v", ErrPermissionDenied, c.socketPath, err)
		case os.IsNotExist(err):
			if _, lookErr := exec.LookPath("docker"); lookErr != nil {
				return VersionInfo{}, fmt.Errorf("%w: %s missing and docker binary not found", ErrNotInstalled, c.socketPath)
			}
			return VersionInfo{}, fmt.Errorf("%w: %s missing (daemon likely stopped)", ErrDaemonDown, c.socketPath)
		default:
			return VersionInfo{}, err
		}
	} else if fi.IsDir() {
		return VersionInfo{}, fmt.Errorf("%w: %s is a directory", ErrNotInstalled, c.socketPath)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/version", nil)
	if err != nil {
		return VersionInfo{}, err
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return VersionInfo{}, translateDialError(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		return VersionInfo{}, fmt.Errorf("%w: docker returned %d", ErrPermissionDenied, resp.StatusCode)
	}
	if resp.StatusCode >= 500 {
		return VersionInfo{}, fmt.Errorf("%w: docker returned %d", ErrDaemonDown, resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return VersionInfo{}, fmt.Errorf("docker: unexpected status %d", resp.StatusCode)
	}

	var body struct {
		Version    string `json:"Version"`
		APIVersion string `json:"ApiVersion"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return VersionInfo{}, fmt.Errorf("docker: decode /version: %w", err)
	}
	return VersionInfo{Version: body.Version, APIVersion: body.APIVersion}, nil
}

func translateDialError(err error) error {
	if errors.Is(err, syscall.EACCES) || errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("%w: %v", ErrPermissionDenied, err)
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT) {
		// A bare ENOENT at dial time also means a missing socket; treat
		// it as daemon-down to avoid mislabelling a stopped daemon as
		// uninstalled (see the os.Stat branch above for the reasoning).
		return fmt.Errorf("%w: %v", ErrDaemonDown, err)
	}
	// Fall back to substring sniffing for wrapped net errors that don't
	// preserve the underlying syscall through errors.Is.
	msg := err.Error()
	switch {
	case strings.Contains(msg, "permission denied"):
		return fmt.Errorf("%w: %v", ErrPermissionDenied, err)
	case strings.Contains(msg, "no such file"),
		strings.Contains(msg, "connection refused"):
		return fmt.Errorf("%w: %v", ErrDaemonDown, err)
	}
	return err
}
