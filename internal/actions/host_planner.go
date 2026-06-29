package actions

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rockclaver/claver-agent/internal/infra"
)

// HostMetricsSource provides a read-only snapshot of host metrics.
// *infra.Manager satisfies it.
type HostMetricsSource interface {
	Sample(ctx context.Context) infra.HostMetrics
}

// HostQueryPlanner is the Phase-1 read-only planner that actually answers
// server-scoped host-resource questions ("how much RAM is left", "disk space",
// "CPU load") from live host metrics. The agent is itself the host, so these
// queries need no target resolution — they always refer to this machine.
//
// Requests with no recognised host-metric intent fall through to
// StatusNeedsTarget, preserving the honest "specify the target" behaviour for
// everything outside host queries (container/service/project work lands when
// the full target resolver is wired on the agent).
type HostQueryPlanner struct {
	Metrics HostMetricsSource
	// Hostname, when set, is included in summaries so the result names the host
	// the user asked about.
	Hostname string
}

// hostIntent is one recognised host-metric question.
type hostIntent int

const (
	intentMemory hostIntent = iota
	intentSwap
	intentDisk
	intentCPU
	intentLoad
)

func (p HostQueryPlanner) Plan(ctx context.Context, req Request) (Result, error) {
	intents := detectHostIntents(req.Text)
	if len(intents) == 0 {
		return Result{
			Status:  StatusNeedsTarget,
			Summary: "couldn't tell which host resource to check; ask about memory, disk, CPU, or load",
			Events: []PlannerEvent{
				{Type: "observation", Message: "host planner: no recognised host-metric intent in request"},
			},
		}, nil
	}
	if p.Metrics == nil {
		return Result{}, errors.New("host planner: metrics source unavailable")
	}

	snap := p.Metrics.Sample(ctx)
	var events []PlannerEvent
	var summaries []string
	for _, in := range intents {
		line, ev := p.observe(in, snap)
		summaries = append(summaries, line)
		events = append(events, ev)
	}

	summary := strings.Join(summaries, "; ")
	if p.Hostname != "" {
		summary = p.Hostname + ": " + summary
	}
	return Result{
		Status:  StatusObserved,
		Summary: summary,
		Events:  events,
	}, nil
}

func (p HostQueryPlanner) observe(in hostIntent, snap infra.HostMetrics) (string, PlannerEvent) {
	switch in {
	case intentMemory:
		m := snap.Memory
		if !m.Available {
			return "memory metrics unavailable", PlannerEvent{Type: "observation", Message: "memory: " + reasonText(m.MetricReason)}
		}
		line := fmt.Sprintf("%s RAM free of %s (%.0f%% used)",
			humanBytes(m.AvailableBytes), humanBytes(m.TotalBytes), m.Percent)
		return line, PlannerEvent{Type: "observation", Message: "read /proc/meminfo: " + line}
	case intentSwap:
		s := snap.Swap
		if !s.Available || s.TotalBytes == 0 {
			return "no swap configured", PlannerEvent{Type: "observation", Message: "swap: none configured"}
		}
		line := fmt.Sprintf("%s swap free of %s (%.0f%% used)",
			humanBytes(s.AvailableBytes), humanBytes(s.TotalBytes), s.Percent)
		return line, PlannerEvent{Type: "observation", Message: "read /proc/meminfo (swap): " + line}
	case intentDisk:
		if len(snap.Disks) == 0 {
			return "disk metrics unavailable", PlannerEvent{Type: "observation", Message: "disk: no mountpoints reported"}
		}
		var parts []string
		for _, d := range snap.Disks {
			if !d.Available {
				parts = append(parts, fmt.Sprintf("%s unavailable", d.Mountpoint))
				continue
			}
			parts = append(parts, fmt.Sprintf("%s %s free of %s (%.0f%% used)",
				d.Mountpoint, humanBytes(d.AvailableBytes), humanBytes(d.TotalBytes), d.Percent))
		}
		line := strings.Join(parts, ", ")
		return line, PlannerEvent{Type: "observation", Message: "statfs mountpoints: " + line}
	case intentCPU:
		c := snap.CPU
		if !c.Available {
			return "CPU metrics unavailable", PlannerEvent{Type: "observation", Message: "cpu: " + reasonText(c.MetricReason)}
		}
		line := fmt.Sprintf("CPU at %.0f%%", c.Percent)
		return line, PlannerEvent{Type: "observation", Message: "sampled /proc/stat: " + line}
	case intentLoad:
		l := snap.Load
		if !l.Available {
			return "load metrics unavailable", PlannerEvent{Type: "observation", Message: "load: " + reasonText(l.MetricReason)}
		}
		line := fmt.Sprintf("load %.2f / %.2f / %.2f (1/5/15m)", l.One, l.Five, l.Fifteen)
		return line, PlannerEvent{Type: "observation", Message: "read /proc/loadavg: " + line}
	}
	return "", PlannerEvent{}
}

func reasonText(r infra.MetricReason) string {
	if r.Message != "" {
		return r.Message
	}
	if r.Reason != "" {
		return r.Reason
	}
	return "unavailable"
}

// detectHostIntents extracts recognised host-metric questions from free text,
// in a deterministic order, de-duplicated.
func detectHostIntents(text string) []hostIntent {
	lower := strings.ToLower(text)
	type probe struct {
		intent   hostIntent
		keywords []string
	}
	// Order defines the order intents appear in the summary.
	probes := []probe{
		{intentMemory, []string{"ram", "memory", "mem "}},
		{intentSwap, []string{"swap"}},
		{intentDisk, []string{"disk", "storage", "space", "filesystem", "df"}},
		{intentCPU, []string{"cpu", "processor"}},
		{intentLoad, []string{"load average", "loadavg", "load"}},
	}
	seen := map[hostIntent]bool{}
	var out []hostIntent
	for _, pr := range probes {
		for _, kw := range pr.keywords {
			if strings.Contains(lower, kw) {
				if !seen[pr.intent] {
					seen[pr.intent] = true
					out = append(out, pr.intent)
				}
				break
			}
		}
	}
	return out
}

// humanBytes renders a byte count as a compact human-readable size.
func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
