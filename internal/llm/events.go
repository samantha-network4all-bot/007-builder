package llm

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
	lastTick      atomic.Int64 // unix nanos of the last on-screen status update

	// In-place status state.
	mu          sync.Mutex
	statusShown bool   // is there a \r-line currently parked at the cursor?
	tailBuf     []byte // sliding window of the last assistant-text characters, for the preview
	currentTool string // tool name currently in flight (shown in status line)
	tty         bool

	verbose bool
}

const tailWindow = 70 // characters of assistant text to show in the status preview

// NewEventSink prepares a fresh sink. When verbose=true, every pi event
// produces a line; otherwise only interesting events do.
func NewEventSink(verbose bool) *EventSink {
	s := &EventSink{
		verbose:   verbose,
		startedAt: time.Now(),
		tty:       isTTY(),
	}
	s.lastTick.Store(time.Now().UnixNano())
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
func (s *EventSink) Handle(line string) {
	if !strings.HasPrefix(line, "{") {
		if s.verbose {
			s.permanent(func() { ui.Note("%s", trim(line, 200)) })
		}
		return
	}
	var ev piEvent
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		if s.verbose {
			s.permanent(func() { ui.Note("non-event: %s", trim(line, 200)) })
		}
		return
	}

	switch ev.Type {
	case "session":
		s.permanent(func() { ui.Note("session opened") })
	case "agent_start":
		s.permanent(func() { ui.Step("agent starting") })
	case "message_update":
		switch ev.AssistantMessageEvent.Type {
		case "text_delta":
			delta := ev.AssistantMessageEvent.Delta
			s.assistantText.WriteString(delta)
			atomic.AddInt64(&s.charsOut, int64(len(delta)))
			s.appendTail(delta)
			s.maybeTick()
		case "text_start":
			// quiet — beginning a new text run; the status line will reflect it
		case "text_end":
			// quiet
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
			s.permanent(func() { ui.Fail("tool error: %s", ev.ToolResultEvent.ToolName) })
		}
	case "agent_end":
		s.permanent(func() {
			ui.OK("agent done · %s out · %s",
				humanChars(atomic.LoadInt64(&s.charsOut)),
				humanDur(time.Since(s.startedAt)))
		})
	default:
		if s.verbose {
			s.permanent(func() { ui.Note("event: %s", ev.Type) })
		}
	}
}

// permanent prints a persistent log line. On a TTY it first clears the
// in-place status line, then runs the print, then re-shows the status
// so the cursor lands back on a clean status row.
func (s *EventSink) permanent(print func()) {
	s.mu.Lock()
	if s.tty && s.statusShown {
		fmt.Print("\r\033[K")
		s.statusShown = false
	}
	s.mu.Unlock()
	print()
	// On TTY, re-arm the status line immediately so the next tick
	// repaints in place (rather than appending after the print).
	s.maybeTickForce()
}

func (s *EventSink) appendTail(delta string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range []byte(delta) {
		// Treat newlines/tabs as spaces so the preview stays on one line.
		if b == '\n' || b == '\r' || b == '\t' {
			b = ' '
		}
		s.tailBuf = append(s.tailBuf, b)
	}
	if len(s.tailBuf) > tailWindow {
		s.tailBuf = s.tailBuf[len(s.tailBuf)-tailWindow:]
	}
}

func (s *EventSink) renderStatus() string {
	chars := atomic.LoadInt64(&s.charsOut)
	elapsed := time.Since(s.startedAt)
	rate := humanRate(chars, elapsed)

	s.mu.Lock()
	tool := s.currentTool
	tail := string(s.tailBuf)
	s.mu.Unlock()

	main := fmt.Sprintf("≈ %s · %s · %s", humanChars(chars), humanDur(elapsed), rate)
	if tool != "" {
		return main + " · tool: " + tool
	}
	if strings.TrimSpace(tail) != "" {
		return main + " · " + tail
	}
	return main + " · thinking…"
}

// AssistantText returns the assembled assistant text after the run
// completes. Useful for callers that need the raw model response
// (e.g. the planner, which expects JSON).
func (s *EventSink) AssistantText() string { return s.assistantText.String() }

// Finish clears the in-place status line so subsequent log output starts
// on a clean row. Idempotent; safe to call multiple times.
func (s *EventSink) Finish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tty && s.statusShown {
		fmt.Print("\r\033[K")
		s.statusShown = false
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
		s.permanent(func() { ui.Step("tool: %s", display) })
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

// maybeTick refreshes the in-place status line at most every 250ms.
// On TTY it uses \r + clear-to-end so the line is overwritten in place;
// on a pipe it falls back to one-line-per-tick, but throttled.
func (s *EventSink) maybeTick() {
	const minInterval = 250 * time.Millisecond
	now := time.Now().UnixNano()
	last := s.lastTick.Load()
	if time.Duration(now-last) < minInterval {
		return
	}
	if !s.lastTick.CompareAndSwap(last, now) {
		return
	}
	s.paintStatus()
}

// maybeTickForce updates the status line right after a permanent event,
// without honoring the rate limit, so the screen always shows a status
// row after a tool-call print.
func (s *EventSink) maybeTickForce() {
	s.lastTick.Store(time.Now().UnixNano())
	s.paintStatus()
}

func (s *EventSink) paintStatus() {
	line := s.renderStatus()
	if s.tty {
		fmt.Printf("\r\033[K\033[2m%s\033[0m", line)
		s.mu.Lock()
		s.statusShown = true
		s.mu.Unlock()
	} else {
		// Non-TTY: one row per tick, throttled to 250ms (above) so logs
		// stay readable when piped.
		fmt.Println(line)
	}
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
