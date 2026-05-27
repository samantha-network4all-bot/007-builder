// Package llm shells out to the pi (preferred) or claude CLI to invoke
// an LLM. It distinguishes the four exit categories enumerated in
// lessons §2.6 so the orchestrator can take the right next action.
package llm

import "fmt"

// Outcome categorises an agent invocation. The orchestrator uses these to
// decide whether to retry, escalate to HITL, or accept the result.
type Outcome int

const (
	OutcomeUnknown Outcome = iota
	OutcomeSucceeded
	OutcomeNoop       // CLI exited 0 but HEAD didn't move
	OutcomeBadOutput  // CLI exited 0 but produced invalid JSON / broken patch
	OutcomeRefused    // CLI exited non-zero with a "I cannot help with that" body
	OutcomeUnreachable // network / upstream error reaching the model
)

// Invocation describes one agent run.
type Invocation struct {
	PromptPath string            // path to the rendered prompt template
	Mode       string            // "code", "review", "plan"
	Env        map[string]string // extra env (e.g. ANTHROPIC_API_KEY)
	WorkingDir string            // chdir before invocation
}

// Run invokes pi (or claude as a fallback) and returns a categorised outcome
// plus the agent's last stdout (for log forwarding).
//
// TODO(slate-v2): implement. Sketch:
//   1. git rev-parse HEAD before.
//   2. `pi --print --no-session --mode json --append-system-prompt @PROMPT --tools read,bash,edit,write,grep,find,ls`.
//   3. Inspect stderr for "Connection error" / "Unauthorized" → Unreachable.
//   4. git rev-parse HEAD after — equal? → Noop.
//   5. Otherwise Succeeded.
func Run(inv Invocation) (Outcome, string, error) {
	return OutcomeUnknown, "", fmt.Errorf("TODO: llm.Run not yet implemented")
}
