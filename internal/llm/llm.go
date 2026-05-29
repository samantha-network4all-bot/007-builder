// Package llm shells out to the pi (preferred) or claude CLI to invoke
// an LLM. It distinguishes the five exit categories enumerated in
// lessons §2.6 so the orchestrator can take the right next action.
package llm

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/samantha-network4all-bot/007-builder/internal/sh"
)

// cavemanPrefix is prepended to UserMessage when Invocation.Caveman is
// set. The phrasing is intentionally blunt — owl-alpha and similar
// chat-tuned models will otherwise default to "Let me analyze…"
// preambles that scroll for minutes before doing anything.
const cavemanPrefix = `CAVEMAN MODE.
Rules:
- No preamble. No "Let me…", "I'll…", "Looking at…", "Based on…".
- No restating the task. No summaries before acting.
- No thinking-out-loud. If you must think, think silently.
- Tool calls and final answer only. Status updates are at most one short line.
- Final answer = the smallest possible artifact (JSON, commit, etc.). No commentary.
- If you start writing prose, stop and emit a tool call instead.`

// Outcome categorises an agent invocation.
type Outcome int

const (
	OutcomeUnknown     Outcome = iota
	OutcomeSucceeded            // CLI exited 0 and (for code agents) HEAD moved
	OutcomeNoop                 // CLI exited 0 but HEAD did not move
	OutcomeBadOutput            // CLI exited 0 but downstream parsing failed
	OutcomeRefused              // CLI exited non-zero with a polite refusal in the body
	OutcomeUnreachable          // network / upstream error reaching the model
)

func (o Outcome) String() string {
	switch o {
	case OutcomeSucceeded:
		return "succeeded"
	case OutcomeNoop:
		return "noop"
	case OutcomeBadOutput:
		return "bad-output"
	case OutcomeRefused:
		return "refused"
	case OutcomeUnreachable:
		return "unreachable"
	default:
		return "unknown"
	}
}

// Invocation describes one agent run.
type Invocation struct {
	CLI              string // "pi" (default) or "claude"
	Mode             string // "json" or "text" (--mode)
	SystemPromptFile string // appended via --append-system-prompt
	UserMessage      string // first user-role message
	Tools            string // --tools allowlist (comma-separated)
	Model            string // --model "<provider>/<id>"
	Thinking         string // --thinking level
	WorkingDir       string // cwd of subprocess
	TrackCommit      bool   // record HEAD before/after; if unchanged on exit 0 → Noop

	// Caveman, when true, prepends a terseness directive to UserMessage
	// so the model produces minimum prose. Useful when the agent's
	// "let me analyze the current state by looking at..." monologues
	// burn tokens and elapsed time without adding signal.
	Caveman bool

	// Skills is a list of skill file paths to load via pi --skill <path>.
	// Each path becomes one --skill argument. Use this to inject
	// architectural contracts (slate's MVC contract, thermo-nuclear
	// review rules) without having to inline them in every prompt.
	Skills []string

	// Stream, when non-nil, switches Run to live stdout pumping. Each
	// stdout line is fed to Stream.Handle as it arrives — useful when
	// pi runs in --mode json and emits structured events the caller
	// wants to render in real time.
	Stream *EventSink
}

// Result is what Run returns alongside the categorised outcome.
type Result struct {
	Outcome     Outcome
	Stdout      string
	Stderr      string
	ExitCode    int
	HEADBefore  string
	HEADAfter   string
	CommandLine string
}

// Run invokes pi (or claude) with the given inputs and returns a
// categorised outcome. The contract is "the process ran with a
// recognisable exit category, or we return an error" — callers should
// branch on Outcome, not ExitCode.
func Run(inv Invocation) (Result, error) {
	cli := inv.CLI
	if cli == "" {
		cli = "pi"
	}

	headBefore := ""
	if inv.TrackCommit {
		if r, err := sh.Run(inv.WorkingDir, "git", "rev-parse", "HEAD"); err == nil && r.ExitCode == 0 {
			headBefore = strings.TrimSpace(r.Stdout)
		}
	}

	args := []string{"--print", "--no-session"}
	if inv.Mode != "" {
		args = append(args, "--mode", inv.Mode)
	}
	if inv.SystemPromptFile != "" {
		args = append(args, "--append-system-prompt", inv.SystemPromptFile)
	}
	if inv.Tools != "" {
		args = append(args, "--tools", inv.Tools)
	}
	for _, sk := range inv.Skills {
		if sk == "" {
			continue
		}
		args = append(args, "--skill", sk)
	}
	if inv.Model != "" {
		args = append(args, "--model", inv.Model)
	}
	if inv.Thinking != "" {
		args = append(args, "--thinking", inv.Thinking)
	}
	userMsg := inv.UserMessage
	if inv.Caveman {
		userMsg = cavemanPrefix + "\n\n" + userMsg
	}
	args = append(args, userMsg)

	res := Result{CommandLine: cli + " " + strings.Join(args, " ")}

	var (
		r   sh.Result
		err error
	)
	if inv.Stream != nil {
		r, err = sh.Stream(inv.WorkingDir, cli, args, inv.Stream.Handle, nil)
	} else {
		r, err = sh.Run(inv.WorkingDir, cli, args...)
	}
	if err != nil {
		res.Outcome = OutcomeUnknown
		return res, err
	}
	res.Stdout = r.Stdout
	res.Stderr = r.Stderr
	res.ExitCode = r.ExitCode
	res.HEADBefore = headBefore

	switch {
	case unreachable(r):
		res.Outcome = OutcomeUnreachable
	case refused(r):
		res.Outcome = OutcomeRefused
	case r.ExitCode != 0:
		res.Outcome = OutcomeBadOutput
	default:
		if inv.TrackCommit {
			if after, ok := readHEAD(inv.WorkingDir); ok {
				res.HEADAfter = after
				if after == headBefore {
					res.Outcome = OutcomeNoop
				} else {
					res.Outcome = OutcomeSucceeded
				}
			} else {
				res.Outcome = OutcomeNoop
			}
		} else {
			res.Outcome = OutcomeSucceeded
		}
	}
	return res, nil
}

func readHEAD(dir string) (string, bool) {
	r, err := sh.Run(dir, "git", "rev-parse", "HEAD")
	if err != nil || r.ExitCode != 0 {
		return "", false
	}
	return strings.TrimSpace(r.Stdout), true
}

func unreachable(r sh.Result) bool {
	if r.ExitCode == 0 {
		return false
	}
	needles := []string{
		"connection error", "connection refused", "timed out", "timeout",
		"unauthorized", "401 ", "503 ", "bad gateway", "network is unreachable",
	}
	hay := strings.ToLower(r.Stderr + " " + r.Stdout)
	for _, n := range needles {
		if strings.Contains(hay, n) {
			return true
		}
	}
	return false
}

func refused(r sh.Result) bool {
	if r.ExitCode == 0 {
		return false
	}
	needles := []string{
		"i cannot help", "i can't help", "i won't", "i refuse", "policy",
	}
	hay := strings.ToLower(r.Stdout)
	for _, n := range needles {
		if strings.Contains(hay, n) {
			return true
		}
	}
	return false
}

// MustFile resolves a prompt path and verifies it exists, returning an
// absolute path. Used by callers passing SystemPromptFile.
func MustFile(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("prompt file %s: %w", path, err)
	}
	if st.IsDir() {
		return "", errors.New("prompt path is a directory: " + path)
	}
	return path, nil
}
