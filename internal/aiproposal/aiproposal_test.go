package aiproposal

import (
	"errors"
	"testing"
	"time"
)

func TestManager_CreateAssignsIDAndPending(t *testing.T) {
	m := New()
	m.Now = func() time.Time { return time.Unix(1700000000, 0) }
	p, err := m.Create(Proposal{Kind: KindServiceAction, Params: map[string]any{"name": "nginx", "action": "restart"}})
	if err != nil {
		t.Fatal(err)
	}
	if p.ID == "" || p.Status != StatusPending {
		t.Fatalf("bad proposal: %+v", p)
	}
	if !p.CreatedAt.Equal(time.Unix(1700000000, 0)) {
		t.Fatalf("created_at not set from Now(): %v", p.CreatedAt)
	}
}

func TestManager_CreateRejectsUnknownKind(t *testing.T) {
	if _, err := New().Create(Proposal{Kind: "bogus"}); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("err = %v", err)
	}
}

func TestManager_ListPreservesOrder(t *testing.T) {
	m := New()
	a, _ := m.Create(Proposal{Kind: KindServiceAction})
	b, _ := m.Create(Proposal{Kind: KindFirewallAdd})
	list := m.List()
	if len(list) != 2 || list[0].ID != a.ID || list[1].ID != b.ID {
		t.Fatalf("bad order: %+v", list)
	}
}

func TestManager_ResolveRejectsDoubleResolve(t *testing.T) {
	m := New()
	p, _ := m.Create(Proposal{Kind: KindServiceAction})
	if _, err := m.Resolve(p.ID, StatusExecuted, "ok", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resolve(p.ID, StatusExecuted, "ok", 1); !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("second resolve err = %v", err)
	}
}

func TestManager_ResolveNotFound(t *testing.T) {
	if _, err := New().Resolve("ghost", StatusDeclined, "", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}
