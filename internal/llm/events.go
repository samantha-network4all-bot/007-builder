package llm

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/samantha-network4all-bot/007-builder/internal/ui"
)

// piEvent is the subset of pi --mode json events we actually care about.
// Pi emits one JSON object per line; unknown event types fall through
// to a generic "saw event: <type>" log.
type piEvent struct {
	Type    string          `json:"type"`
	Message json.RawMessage `json:"message"`

	// Streaming assistant text comes through assistantMessageEvent.
	AssistantMessageEvent struct {
		Type         string          `json:"type"`         // text_start | text_delta | text_end
		ContentIndex int             `json:"contentIndex"`
		Delta        string          `json:"delta"`
		Partial      json.RawMessage `json:"partial"`
	} `json:"assistantMessageEvent"`

	// Tool calls come through toolUseEvent (start/delta/end).
	ToolUseEvent struct {
		Type      string          `json:"type"`
		ToolName  string          `json:"toolName"`
		ToolInput json.RawMessage `json:"toolInput"`
	} `json:"toolUseEvent"`

	// Tool results.
	ToolResultEvent struct {
		Type     string          `json:"type"`
		ToolName string          `json:"toolName"`
		Output   json.RawMessage `json:"output"`
		IsError  bool            `json:"isError"`
	} `json:"toolResultEvent"`
}

// EventSink collates streaming pi events into something the orchestrator
// can use after the run finishes. The sink:
//   - prints a colourful "what's happening now" line for each event,
//   - assembles the assistant's final text from text_delta events,
//   - tracks elapsed time + a rough character/token counter so the user
//     can see the agent is alive even during long thinks.
type EventSink struct {
	assistantText strings.Builder
	charsOut      int64
	startedAt     time.Time
	lastTick      atomic.Int64 // unix nanos of the last on-screen status update
	lastTool      string       // most recent tool name, for in-progress hints
	verbose       bool
}

// NewEventSink prepares a fresh sink. When verbose=true, every pi event
// produces a line; otherwise only "interesting" events do (tool calls,
// turn boundaries, errors, errors).
func NewEventSink(verbose bool) *EventSink {
	s := &EventSink{verbose: verbose, startedAt: time.Now()}
	s.lastTick.Store(time.Now().UnixNano())
	return s
}

// Handle processes one stdout line from pi. Returns silently if the
// line doesn't look like JSON (some pi modes interleave plain text).
func (s *EventSink) Handle(line string) {
	if !strings.HasPrefix(line, "{") {
		// Plain text — could be a stderr-style notice that pi sent to
		// stdout. Dim-print so the user sees something.
		if s.verbose {
			ui.Note("%s", trim(line, 200))
		}
		return
	}
	var ev piEvent
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		if s.verbose {
			ui.Note("non-event: %s", trim(line, 200))
		}
		return
	}

	switch ev.Type {
	case "session":
		ui.Note("session opened")
	case "agent_start":
		ui.Step("agent starting")
	case "turn_start":
		// quiet — we already announced the agent
	case "message_start":
		// quiet
	case "message_update":
		// Accumulate text deltas; print a "tokens so far" tick at most once per second.
		switch ev.AssistantMessageEvent.Type {
		case "text_delta":
			s.assistantText.WriteString(ev.AssistantMessageEvent.Delta)
			atomic.AddInt64(&s.charsOut, int64(len(ev.AssistantMessageEvent.Delta)))
			s.maybeTick()
		case "text_start":
			ui.Step("model speaking")
		case "text_end":
			// quiet — final text will be in the sink for downstream use
		}
		// Tool-use events sometimes ride on message_update too; surface those.
		if ev.ToolUseEvent.Type != "" {
			s.handleToolUse(ev.ToolUseEvent.Type, ev.ToolUseEvent.ToolName, ev.ToolUseEvent.ToolInput)
		}
	case "message_end":
		// quiet
	case "tool_use_start", "tool_use":
		s.handleToolUse("start", ev.ToolUseEvent.ToolName, ev.ToolUseEvent.ToolInput)
	case "tool_use_end":
		s.handleToolUse("end", ev.ToolUseEvent.ToolName, nil)
	case "tool_result_start":
		// quiet
	case "tool_result_end":
		if ev.ToolResultEvent.IsError {
			ui.Fail("tool error: %s", ev.ToolResultEvent.ToolName)
		}
	case "agent_end":
		ui.OK("agent done · %s · %s",
			humanRate(atomic.LoadInt64(&s.charsOut), time.Since(s.startedAt)),
			humanDur(time.Since(s.startedAt)))
	case "turn_end":
		// quiet
	default:
		if s.verbose {
			ui.Note("event: %s", ev.Type)
		}
	}
}

// AssistantText returns the assembled assistant text after the run
// completes. Useful for callers that need the raw model response
// (e.g. the planner, which expects JSON).
func (s *EventSink) AssistantText() string { return s.assistantText.String() }

func (s *EventSink) handleToolUse(kind, name string, input json.RawMessage) {
	if name == "" {
		return
	}
	switch kind {
	case "start", "":
		summary := toolSummary(name, input)
		if summary != "" {
			ui.Step("tool: %s(%s)", name, summary)
		} else {
			ui.Step("tool: %s", name)
		}
		s.lastTool = name
	case "end":
		// keep silent — too noisy to print every result
	}
}

// toolSummary produces a one-line abbreviation of a tool's input args
// suitable for inline display.
func toolSummary(name string, input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(input, &m); err != nil {
		return trim(string(input), 60)
	}
	// Pick the most distinguishing field per tool.
	picks := map[string]string{
		"read":  "path",
		"edit":  "path",
		"write": "path",
		"grep":  "pattern",
		"find":  "pattern",
		"ls":    "path",
		"bash":  "command",
	}
	if k, ok := picks[name]; ok {
		if v, ok := m[k]; ok {
			return trim(fmt.Sprint(v), 80)
		}
	}
	// Fallback: first short value.
	for _, v := range m {
		s := fmt.Sprint(v)
		if len(s) > 0 && len(s) < 100 {
			return s
		}
	}
	return trim(string(input), 60)
}

// maybeTick prints "≈ Nk chars · Ts" at most once per ~750ms so the
// user sees forward motion without flooding stdout.
func (s *EventSink) maybeTick() {
	const minInterval = 750 * time.Millisecond
	now := time.Now().UnixNano()
	last := s.lastTick.Load()
	if time.Duration(now-last) < minInterval {
		return
	}
	if !s.lastTick.CompareAndSwap(last, now) {
		return
	}
	c := atomic.LoadInt64(&s.charsOut)
	elapsed := time.Since(s.startedAt)
	ui.Note("≈ %s out · %s elapsed · %s", humanChars(c), humanDur(elapsed), humanRate(c, elapsed))
}

func trim(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func humanChars(n int64) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d chars", n)
	case n < 100_000:
		return fmt.Sprintf("%.1fk chars", float64(n)/1000)
	default:
		return fmt.Sprintf("%dk chars", n/1000)
	}
}

func humanDur(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
}

func humanRate(chars int64, d time.Duration) string {
	if d <= 0 {
		return "—"
	}
	cps := float64(chars) / d.Seconds()
	switch {
	case cps < 50:
		return fmt.Sprintf("%.0f c/s", cps)
	default:
		return fmt.Sprintf("%.0f c/s", cps)
	}
}
