package sessions

// structured.go holds the agent-neutral infrastructure shared by every
// structured runtime (Claude, Codex). Each runtime translates its native CLI
// protocol into the normalized events of normalize.go and publishes them
// through a structuredSink; the translate step produces []translated, which the
// sink fans out to the Manager's persisted/ephemeral publish paths.

import "github.com/rockclaver/claver-agent/internal/store"

// translated is one normalized event a runtime will publish. Ephemeral events
// (streaming deltas) are delivered live-only; the rest are persisted.
type translated struct {
	Type      string
	Payload   any
	Ephemeral bool
}

// structuredSink publishes normalized events for one session back to the
// Manager. It is shared by the Claude and Codex structured runtimes.
type structuredSink struct {
	sessionID string
	emit      func(store.SessionEvent) // persisted
	ephemeral func(store.SessionEvent) // live-only
}

func (s structuredSink) publish(tr translated) {
	ev, err := normalizedEvent(s.sessionID, tr.Type, tr.Payload)
	if err != nil {
		return
	}
	if tr.Ephemeral {
		if s.ephemeral != nil {
			s.ephemeral(ev)
		}
		return
	}
	if s.emit != nil {
		s.emit(ev)
	}
}

func (s structuredSink) publishError(msg string, fatal bool) {
	if s.emit == nil {
		return
	}
	ev, err := normalizedEvent(s.sessionID, EvError, ErrorEvent{Message: msg, Fatal: fatal})
	if err == nil {
		s.emit(ev)
	}
}
