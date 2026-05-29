// Package stream renders pi's live event stream as colored, scrolling
// terminal output. It is general-purpose subprocess-streaming
// infrastructure — moved out of internal/llm because pi is the first
// consumer but no longer the only conceivable one (a future build
// monitor, log tailer, etc. could use the same sink).
//
// Consumers pass *EventSink as a callback target to long-running
// processes (typically via internal/llm.Invocation.Stream).
package stream

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
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
//   - prints a single in-place status line (chars · elapsed · c/s · tail-preview)
//     that updates every ~250ms via carriage-return — no scroll-spam,
//   - prints permanent log lines (tool calls, errors, completion) on their
//     own rows, clearing the status line first and reprinting it after,
//   - assembles the assistant's final text from text_delta events,
//   - falls back to one-line-per-tick on non-TTY stdout so piped logs
//     still capture the timeline.
type EventSink struct {
	assistantText strings.Builder
	charsOut      int64
	startedAt     time.Time
	mu           sync.Mutex
	inTextStream bool   // are we currently mid-paragraph of dim model text?
	currentTool  string // last tool that was started, for log context
	tty          bool

	verbose bool
}

// NewEventSink prepares a fresh sink. When verbose=true, every pi event
// produces a line; otherwise only interesting events do.
func NewEventSink(verbose bool) *EventSink {
	s := &EventSink{
		verbose:   verbose,
		startedAt: time.Now(),
		tty:       isTTY(),
	}
	return s
}

func isTTY() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// Handle processes one stdout line from pi.
//
// Design: stream the model's text deltas directly to stdout in dim
// color so the user can read along. Tool calls pop out as bright
// permanent events above the dim stream. There is no in-place status
// counter — the visible text *is* the proof of life. A summary line
// prints at agent_end with chars/elapsed/rate.
func (s *EventSink) Handle(line string) {
	if !strings.HasPrefix(line, "{") {
		return
	}
	var ev piEvent
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return
	}

	switch ev.Type {
	case "session":
		s.eventLine(func() { ui.Note("session opened") })
	case "agent_start":
		s.eventLine(func() { ui.Step("agent starting") })
	case "message_update":
		switch ev.AssistantMessageEvent.Type {
		case "text_delta":
			delta := ev.AssistantMessageEvent.Delta
			s.assistantText.WriteString(delta)
			atomic.AddInt64(&s.charsOut, int64(len(delta)))
			s.streamText(delta)
		case "text_start":
			// nothing — the delta itself is the cue
		case "text_end":
			// keep the cursor at end-of-line; next text run will resume
		}
		if ev.ToolUseEvent.Type != "" {
			s.handleToolUse(ev.ToolUseEvent.Type, ev.ToolUseEvent.ToolName, ev.ToolUseEvent.ToolInput)
		}
	case "tool_use_start", "tool_use":
		s.handleToolUse("start", ev.ToolUseEvent.ToolName, ev.ToolUseEvent.ToolInput)
	case "tool_use_end":
		s.handleToolUse("end", ev.ToolUseEvent.ToolName, nil)
	case "tool_result_end":
		if ev.ToolResultEvent.IsError {
			s.eventLine(func() { ui.Fail("tool error: %s", ev.ToolResultEvent.ToolName) })
		}
	case "agent_end":
		s.eventLine(func() {
			elapsed := time.Since(s.startedAt)
			c := atomic.LoadInt64(&s.charsOut)
			ui.OK("agent done · %s · %s · %s", humanChars(c), humanDur(elapsed), humanRate(c, elapsed))
		})
	default:
		// silent — too noisy
	}
}

// streamText writes a model-text delta directly to stdout in dim color,
// finishing whatever line is in flight. Newlines are preserved so the
// model's paragraph structure shows through.
func (s *EventSink) streamText(delta string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.inTextStream {
		// Start of a new text run — drop a newline + dim escape so the
		// stream visually sits below the last bright event line.
		fmt.Print("\033[2m")
		s.inTextStream = true
	}
	fmt.Print(delta)
}

// eventLine ends the current dim text stream (if any) and prints a
// permanent event line via ui. Subsequent text deltas restart the dim
// stream.
func (s *EventSink) eventLine(print func()) {
	s.mu.Lock()
	if s.inTextStream {
		fmt.Print("\033[0m\n")
		s.inTextStream = false
	}
	s.mu.Unlock()
	print()
}

// AssistantText returns the assembled assistant text after the run
// completes. Useful for callers that need the raw model response
// (e.g. the planner, which expects JSON).
func (s *EventSink) AssistantText() string { return s.assistantText.String() }

// Finish closes any open dim text stream so subsequent log output
// starts on a clean row with the default color. Idempotent.
func (s *EventSink) Finish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inTextStream {
		fmt.Print("\033[0m\n")
		s.inTextStream = false
	}
}

func (s *EventSink) handleToolUse(kind, name string, input json.RawMessage) {
	if name == "" {
		return
	}
	switch kind {
	case "start", "":
		summary := toolSummary(name, input)
		display := name
		if summary != "" {
			display = name + "(" + summary + ")"
		}
		s.mu.Lock()
		s.currentTool = display
		s.mu.Unlock()
		s.eventLine(func() { ui.Step("tool: %s", display) })
	case "end":
		s.mu.Lock()
		s.currentTool = ""
		s.mu.Unlock()
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
