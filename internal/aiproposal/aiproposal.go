// Package aiproposal owns the AI Proposal Queue deep module. An AI session
// running on the host cannot mutate state directly; instead it submits a
// concrete, parameterised proposal (e.g. "restart nginx.service") which is
// surfaced to the operator as an approval card. The operator approves via the
// existing biometric -> confirmation-token path, and only then is the action
// executed through the same managers (systemd / process / firewall) that serve
// human-initiated calls. Audit rows for the resulting action are attributed to
// the "ai-proposed" actor so the audit log records that the side effect was
// machine-suggested but human-approved.
package aiproposal

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"
)

// Kind enumerates the proposal kinds the AI may submit. Each one maps 1:1 to
// an existing infra mutation kind on the wire, so the same guards and audit
// paths apply.
type Kind string

const (
	KindServiceAction  Kind = "infra.service.action"
	KindProcessKill    Kind = "infra.process.kill"
	KindFirewallAdd    Kind = "infra.firewall.rule_add"
	KindFirewallRemove Kind = "infra.firewall.rule_remove"
)

// Status is the lifecycle state of a proposal.
type Status string

const (
	StatusPending  Status = "pending"
	StatusExecuted Status = "executed"
	StatusDeclined Status = "declined"
	StatusRejected Status = "rejected" // guard or runtime rejected after approve attempt
	StatusFailed   Status = "failed"   // execution error after token consumed
)

// Errors returned by the Manager.
var (
	ErrNotFound       = errors.New("proposal not found")
	ErrAlreadyResolved = errors.New("proposal already resolved")
	ErrUnknownKind    = errors.New("unknown proposal kind")
)

// Proposal is one entry in the queue.
type Proposal struct {
	ID         string         `json:"id"`
	Kind       Kind           `json:"kind"`
	Params     map[string]any `json:"params"`
	Rationale  string         `json:"rationale"`
	SessionID  string         `json:"session_id,omitempty"`
	// TokenAction is the action string the confirmation token must bind to.
	// The operator uses (TokenAction, TokenProjectID, TokenFiles) to mint a
	// token whose action_hash matches what the server will check at approve
	// time. This is exposed so the mobile client can compute the same hash.
	TokenAction    string    `json:"token_action"`
	TokenProjectID string    `json:"token_project_id"`
	TokenFiles     []string  `json:"token_files"`
	Status         Status    `json:"status"`
	CreatedAt      time.Time `json:"-"`
	ResolvedAt     *time.Time `json:"-"`
	ResolutionMsg  string    `json:"resolution_msg,omitempty"`
	AuditID        int64     `json:"audit_id,omitempty"`
}

// Manager owns the in-memory queue of pending and resolved proposals. It is
// process-local: proposals do not survive an agent restart, which matches the
// short-lived nature of an AI session and the requirement that proposals are
// always re-confirmed by a fresh biometric prompt.
type Manager struct {
	Now    func() time.Time
	randID func() string

	mu        sync.Mutex
	proposals map[string]*Proposal
	order     []string
}

// New constructs an empty Manager.
func New() *Manager {
	return &Manager{
		Now:       time.Now,
		randID:    randomID,
		proposals: map[string]*Proposal{},
	}
}

func randomID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Create stores a new pending proposal. The caller is responsible for
// computing TokenAction / TokenProjectID / TokenFiles from the proposal's kind
// and params (so the same binding the human flow uses is reused verbatim).
func (m *Manager) Create(p Proposal) (Proposal, error) {
	switch p.Kind {
	case KindServiceAction, KindProcessKill, KindFirewallAdd, KindFirewallRemove:
	default:
		return Proposal{}, ErrUnknownKind
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p.ID = m.randID()
	p.Status = StatusPending
	p.CreatedAt = m.Now()
	stored := p
	m.proposals[p.ID] = &stored
	m.order = append(m.order, p.ID)
	return stored, nil
}

// Get returns a copy of one proposal by ID.
func (m *Manager) Get(id string) (Proposal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.proposals[id]
	if !ok {
		return Proposal{}, ErrNotFound
	}
	return *p, nil
}

// List returns all proposals in submission order (oldest first).
func (m *Manager) List() []Proposal {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Proposal, 0, len(m.order))
	for _, id := range m.order {
		if p, ok := m.proposals[id]; ok {
			out = append(out, *p)
		}
	}
	return out
}

// Resolve marks a pending proposal as having reached terminal status. It
// returns ErrAlreadyResolved if the proposal has already been resolved, so
// concurrent approval attempts cannot drive a single proposal to execute
// twice.
func (m *Manager) Resolve(id string, status Status, msg string, auditID int64) (Proposal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.proposals[id]
	if !ok {
		return Proposal{}, ErrNotFound
	}
	if p.Status != StatusPending {
		return Proposal{}, ErrAlreadyResolved
	}
	now := m.Now()
	p.Status = status
	p.ResolvedAt = &now
	p.ResolutionMsg = msg
	p.AuditID = auditID
	return *p, nil
}

// SortedFiles returns a defensive copy of files in canonical (sorted) order so
// the confirmation token's action_hash is deterministic across callers.
func SortedFiles(files []string) []string {
	out := append([]string(nil), files...)
	sort.Strings(out)
	return out
}
