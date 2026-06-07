package sessions

import "testing"

const multiGroupMenu = `╭───────────────────────────────────────────╮
│ □ Skills to build   □ Hygiene skills   ✓ Submit
│
│ Which new skills should I build out as full SKILL.md folders?
│
│ ❯ 1. [ ] fix-build
│      Repair broken builds: compile/bundler errors.
│   2. [ ] debug-bisect
│      Drive git bisect to find the regression.
│   3. [x] reproduce-bug
│      Turn a bug report into a minimal repro + failing test.
│   4. [ ] fix-ci
│      Triage and fix a failing CI/PR check, then push.
│   5. [ ] Type something
│      Next
│
│   6. Chat about this
╰───────────────────────────────────────────╯
Enter to select · Tab/Arrow keys to navigate · Esc to cancel`

const singleSelectMenu = `Ready to code?

❯ 1. Yes, proceed
  2. No, keep planning

Enter to select · Tab/Arrow keys to navigate · Esc to cancel`

func TestParseAskQuestion_MultiSelectMultiGroup(t *testing.T) {
	q, ok := parseAskQuestion(multiGroupMenu)
	if !ok {
		t.Fatal("expected menu to parse")
	}
	if !q.MultiSelect {
		t.Error("expected multiSelect=true")
	}
	if !q.AllowFreeText {
		t.Error("expected allowFreeText=true (Type something present)")
	}
	if q.GroupCount != 2 {
		t.Errorf("groupCount=%d, want 2", q.GroupCount)
	}
	if q.Header != "Which new skills should I build out as full SKILL.md folders?" {
		t.Errorf("unexpected header: %q", q.Header)
	}
	if len(q.Options) != 4 {
		t.Fatalf("got %d options, want 4 (Type something + Chat excluded)", len(q.Options))
	}
	if q.Options[0].Index != 1 || q.Options[0].Label != "fix-build" {
		t.Errorf("opt0 = %+v", q.Options[0])
	}
	if q.Options[0].Description != "Repair broken builds: compile/bundler errors." {
		t.Errorf("opt0 description = %q", q.Options[0].Description)
	}
	if q.Options[3].Index != 4 || q.Options[3].Label != "fix-ci" {
		t.Errorf("opt3 = %+v", q.Options[3])
	}
	if q.ID == "" {
		t.Error("expected non-empty content hash id")
	}
}

func TestParseAskQuestion_SingleSelect(t *testing.T) {
	q, ok := parseAskQuestion(singleSelectMenu)
	if !ok {
		t.Fatal("expected menu to parse")
	}
	if q.MultiSelect {
		t.Error("expected multiSelect=false for radio menu")
	}
	if q.GroupCount != 1 {
		t.Errorf("groupCount=%d, want 1", q.GroupCount)
	}
	if len(q.Options) != 2 {
		t.Fatalf("got %d options, want 2", len(q.Options))
	}
	if q.Options[1].Label != "No, keep planning" {
		t.Errorf("opt1 label = %q", q.Options[1].Label)
	}
}

func TestParseAskQuestion_NoFooterRejected(t *testing.T) {
	if _, ok := parseAskQuestion("just some regular terminal output\n1. not a menu\n"); ok {
		t.Error("expected non-menu text to be rejected")
	}
}

func TestParseRows_CursorAndChecked(t *testing.T) {
	rows := parseRows(multiGroupMenu)
	if len(rows) != 6 {
		t.Fatalf("got %d rows, want 6 (incl Type something + Chat)", len(rows))
	}
	if !rows[0].cursor {
		t.Error("row 0 should hold the caret")
	}
	if rows[1].cursor {
		t.Error("row 1 should not hold the caret")
	}
	if !rows[2].checked {
		t.Error("row 2 (reproduce-bug [x]) should be checked")
	}
	if rows[0].checked {
		t.Error("row 0 (fix-build [ ]) should be unchecked")
	}
	if !rows[0].hasBox {
		t.Error("row 0 should be a checkbox")
	}
}

func TestContentHash_StableAndDistinct(t *testing.T) {
	q1, _ := parseAskQuestion(multiGroupMenu)
	q2, _ := parseAskQuestion(multiGroupMenu)
	if q1.ID != q2.ID {
		t.Error("same screen should hash identically")
	}
	q3, _ := parseAskQuestion(singleSelectMenu)
	if q1.ID == q3.ID {
		t.Error("different menus should hash differently")
	}
}
