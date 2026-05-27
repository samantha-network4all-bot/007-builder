// Package github wraps the `gh` CLI for issue, PR, and label operations.
// All calls shell out to gh so we inherit the user's auth and don't need
// a PAT in the orchestrator.
package github

import "fmt"

// Issue is the subset of fields we read/write.
type Issue struct {
	Number int      `json:"number"`
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	State  string   `json:"state"`
	Labels []string `json:"labels"`
}

// ListSlices returns all `slice`-labelled issues (open + closed).
//
// TODO(slate-v2): implement via `gh issue list --label slice --state all --json number,title,body,state,labels --limit 200`.
func ListSlices() ([]Issue, error) {
	return nil, fmt.Errorf("TODO: ListSlices not yet implemented")
}

// CreateIssue opens a new issue. Returns its number.
//
// TODO(slate-v2): implement via `gh issue create --title ... --body ... --label ...`.
func CreateIssue(title, body string, labels []string) (int, error) {
	return 0, fmt.Errorf("TODO: CreateIssue not yet implemented")
}

// BumpAttemptLabel removes `attempt:N` and adds `attempt:N+1`. Returns the
// new attempt number.
//
// TODO(slate-v2): implement via two `gh issue edit --remove-label / --add-label` calls.
func BumpAttemptLabel(issue int) (int, error) {
	return 0, fmt.Errorf("TODO: BumpAttemptLabel not yet implemented")
}

// HandoffForReview opens a PR with `awaiting-human-review` and closes the
// underlying issue (per the HITL pattern in lessons §2).
//
// TODO(slate-v2): implement.
func HandoffForReview(issue int, branch string) error {
	return fmt.Errorf("TODO: HandoffForReview not yet implemented")
}
