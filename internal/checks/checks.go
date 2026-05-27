// Package checks owns the two post-commit gates: code-quality (LLM
// review) and feature-test (build + HTTP probes against the running
// Slate binary).
package checks

import "fmt"

// Dispatch routes `slate-orchestrator check <subcommand> ...` to the right
// handler.
func Dispatch(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: check <quality|feature> <issue>")
	}
	switch args[0] {
	case "quality":
		return Quality(args[1:])
	case "feature":
		return Feature(args[1:])
	default:
		return fmt.Errorf("unknown check subcommand: %q", args[0])
	}
}

// Quality runs the code-quality agent (read-only pi invocation) against the
// open PR for the given issue. Files blocking comments via
// `gh pr review --request-changes` on any PRD §8 violation.
//
// TODO(slate-v2): implement.
func Quality(args []string) error {
	return fmt.Errorf("TODO: check quality not yet implemented")
}

// Feature builds the Slate app, launches it with SLATE_TEST_API=1, polls
// /healthz on the port written to ~/Library/Application Support/Slate/test-api.port,
// then runs the acceptance probes from the issue body. Writes a JSON
// result to .slate/checks/<issue>-feature.json.
//
// TODO(slate-v2): implement.
func Feature(args []string) error {
	return fmt.Errorf("TODO: check feature not yet implemented")
}
