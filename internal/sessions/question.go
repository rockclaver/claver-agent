package sessions

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// AskQuestion is the structured form of one on-screen question group from an
// interactive selector (Claude Code's AskUserQuestion / ExitPlanMode menus).
// It is parsed from a resolved tmux capture-pane snapshot and shipped to the
// mobile client as an "ask_question" event so it can render a native sheet
// instead of forcing the user to drive the raw TUI.
type AskQuestion struct {
	ID            string           `json:"id"`
	GroupIndex    int              `json:"groupIndex"`
	GroupCount    int              `json:"groupCount"`
	Header        string           `json:"header"`
	MultiSelect   bool             `json:"multiSelect"`
	AllowFreeText bool             `json:"allowFreeText"`
	Options       []QuestionOption `json:"options"`
}

type QuestionOption struct {
	Index       int    `json:"index"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

// Key sequences the selector understands, sent verbatim via SendInput (which
// hex-encodes them for tmux send-keys).
const (
	keyDown  = "\x1b[B"
	keyUp    = "\x1b[A"
	keyTab   = "\t"
	keyEnter = "\r"
	keySpace = " "
	keyEsc   = "\x1b"
)

var (
	// Footer rendered beneath every selector. Content-independent, so it is the
	// reliable trigger/confirmation that a menu is on screen.
	questionFooterRE = regexp.MustCompile(`(?i)enter to select.*(?:navigate|cancel)`)
	// One menu row: optional cursor marker, an index, an optional [ ]/[x]
	// checkbox, then the label.
	optionLineRE = regexp.MustCompile(`^\s*([>❯])?\s*(\d+)[.)]\s+(\[[^\]]*\]\s*)?(.*\S)\s*$`)
	// Question tabs in the top bar use box glyphs (□ ☐ ▢ ◻ ◼ ■); the Submit
	// affordance uses a check. Count the box glyphs to learn the group count.
	tabGlyphRE = regexp.MustCompile(`[\x{25A1}\x{2610}\x{25A2}\x{25FB}\x{25FC}\x{25A0}]`)
	// Leading/trailing box-drawing border the selector frame draws around rows.
	borderRE = regexp.MustCompile(`^[\s\x{2500}-\x{257F}|]+|[\s\x{2500}-\x{257F}|]+$`)
)

// parsedRow is the live state of a single selector row, used by the driver to
// navigate and toggle by observing the resolved screen after each keystroke.
type parsedRow struct {
	index   int
	cursor  bool // the ❯/> caret is on this row
	hasBox  bool // row is a checkbox (=> multi-select group)
	checked bool // checkbox is filled
	label   string
}

func stripBorders(s string) string { return borderRE.ReplaceAllString(s, "") }

func cleanLabel(s string) string { return strings.TrimSpace(stripBorders(s)) }

func checkboxChecked(box string) bool {
	inner := strings.TrimSpace(strings.Trim(strings.TrimSpace(box), "[]"))
	return inner != ""
}

func isFreeTextLabel(s string) bool {
	return strings.EqualFold(strings.TrimSpace(s), "type something")
}

func isChatLabel(s string) bool {
	return strings.EqualFold(strings.TrimSpace(s), "chat about this")
}

// parseRows extracts every selectable row from a capture, preserving cursor and
// checkbox state. It stops at the footer so trailing chrome is ignored.
func parseRows(capture string) []parsedRow {
	var rows []parsedRow
	for _, raw := range strings.Split(capture, "\n") {
		ln := stripBorders(raw)
		if questionFooterRE.MatchString(ln) {
			break
		}
		m := optionLineRE.FindStringSubmatch(ln)
		if m == nil {
			continue
		}
		idx, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		rows = append(rows, parsedRow{
			index:   idx,
			cursor:  m[1] != "",
			hasBox:  m[3] != "",
			checked: m[3] != "" && checkboxChecked(m[3]),
			label:   cleanLabel(m[4]),
		})
	}
	return rows
}

// parseAskQuestion turns a resolved capture-pane snapshot into the structured
// question for the currently visible group. ok is false when the snapshot is
// not a selector (no footer) or has no real options.
func parseAskQuestion(capture string) (q *AskQuestion, ok bool) {
	if !questionFooterRE.MatchString(capture) {
		return nil, false
	}
	lines := strings.Split(capture, "\n")

	groupCount := 1
	for _, ln := range lines {
		if strings.Contains(ln, "Submit") && tabGlyphRE.MatchString(ln) {
			if n := len(tabGlyphRE.FindAllString(ln, -1)); n > 0 {
				groupCount = n
			}
			break
		}
	}

	res := &AskQuestion{GroupCount: groupCount}
	headerSet := false
	lastIdx := -1 // index into res.Options for description continuation

	for _, raw := range lines {
		ln := stripBorders(raw)
		if strings.TrimSpace(ln) == "" {
			continue
		}
		if questionFooterRE.MatchString(ln) {
			break
		}
		m := optionLineRE.FindStringSubmatch(ln)
		if m == nil {
			t := cleanLabel(ln)
			if t == "" {
				continue
			}
			if !headerSet && len(res.Options) == 0 && !isChromeLine(ln) {
				res.Header = t
				headerSet = true
			} else if lastIdx >= 0 {
				if res.Options[lastIdx].Description == "" {
					res.Options[lastIdx].Description = t
				} else {
					res.Options[lastIdx].Description += " " + t
				}
			}
			continue
		}

		idx, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		label := cleanLabel(m[4])
		if m[3] != "" {
			res.MultiSelect = true
		}
		if isFreeTextLabel(label) {
			res.AllowFreeText = true
			lastIdx = -1
			continue
		}
		if isChatLabel(label) {
			lastIdx = -1
			continue
		}
		res.Options = append(res.Options, QuestionOption{Index: idx, Label: label})
		lastIdx = len(res.Options) - 1
	}

	if len(res.Options) == 0 {
		return nil, false
	}
	res.ID = contentHash(res.Header, res.Options)
	return res, true
}

// isChromeLine reports whether a non-option line is the tab bar (and thus not
// the question header).
func isChromeLine(ln string) bool {
	return strings.Contains(ln, "Submit") && tabGlyphRE.MatchString(ln)
}

func contentHash(header string, opts []QuestionOption) string {
	var b strings.Builder
	b.WriteString(header)
	for _, o := range opts {
		fmt.Fprintf(&b, "\x00%d:%s", o.Index, o.Label)
	}
	sum := sha1.Sum([]byte(b.String()))
	return hex.EncodeToString(sum[:])[:16]
}
