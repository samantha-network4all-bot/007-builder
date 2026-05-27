// Package plan asks the LLM to choose the next vertical slice given the
// PRD and the list of already-closed slices.
package plan

import "fmt"

// NextIssue reads PRD.md, queries the LLM for the next smallest user-visible
// vertical slice, and opens a GitHub issue with `slice` + `attempt:0` labels.
//
// TODO(slate-v2): implement.
//
// Sketch:
//   1. Read PRD.md from cwd.
//   2. Read closed slice issues via gh: `gh issue list --state closed --label slice --json number,title,body --limit 200`.
//   3. Render orchestrator/prompts/PROMPT-next-issue.tmpl with {PRD, ClosedSlices}.
//   4. Invoke pi --print --no-session --mode json with the rendered prompt;
//      expect JSON {title, body, acceptance:[...]}.
//   5. Open issue: `gh issue create --title "Sn: ..." --body <body> --label slice --label attempt:0`.
//   6. Print issue number to stdout for the caller (work) to pick up.
func NextIssue(args []string) error {
	return fmt.Errorf("TODO: next-issue not yet implemented (see %s comment)", "plan.NextIssue")
}
