// Package github wraps the `gh` CLI for issue, PR, and repo operations.
// All calls shell out to gh so we inherit the user's auth without
// requiring a PAT in the binary.
package github

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/samantha-network4all-bot/007-builder/internal/sh"
)

// Issue is the subset of fields we read/write.
type Issue struct {
	Number int     `json:"number"`
	Title  string  `json:"title"`
	Body   string  `json:"body"`
	State  string  `json:"state"`
	Labels []Label `json:"labels"`
}

// Label is gh's JSON shape for issue labels.
type Label struct {
	Name string `json:"name"`
}

// HasLabel reports whether the issue carries a label with the given name.
func (i Issue) HasLabel(name string) bool {
	for _, l := range i.Labels {
		if l.Name == name {
			return true
		}
	}
	return false
}

// EnsureLabel creates a repo label if it doesn't exist. Errors (including
// "label already exists") are swallowed — callers treat this as
// best-effort.
func EnsureLabel(name string) {
	if name == "" {
		return
	}
	color := "ededed"
	desc := ""
	switch {
	case strings.HasPrefix(name, "attempt:"):
		color, desc = "ededed", "code-agent attempt counter"
	case name == "slice":
		color, desc = "0e8a16", "iterative build slice"
	case strings.Contains(name, "review"):
		color, desc = "d73a4a", "needs human review"
	}
	_, _ = sh.Run("", "gh", "label", "create", name, "--description", desc, "--color", color)
}

// RequireAuth verifies that `gh auth status` succeeds. Returns an error
// otherwise, so Bootstrap can refuse to proceed without auth.
func RequireAuth() error {
	r, err := sh.Run("", "gh", "auth", "status")
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return fmt.Errorf("gh not authenticated:\n%s", r.Combined())
	}
	return nil
}

// RepoExists returns true if `gh repo view <slug>` succeeds.
func RepoExists(slug string) (bool, error) {
	r, err := sh.Run("", "gh", "repo", "view", slug)
	if err != nil {
		return false, err
	}
	if r.ExitCode == 0 {
		return true, nil
	}
	if strings.Contains(r.Stderr, "Could not resolve") ||
		strings.Contains(r.Stderr, "not found") ||
		strings.Contains(r.Stderr, "HTTP 404") {
		return false, nil
	}
	return false, fmt.Errorf("gh repo view %s exited %d:\n%s", slug, r.ExitCode, r.Combined())
}

// CreateRepo creates a public GitHub repo. Caller should check RepoExists
// first for idempotency.
func CreateRepo(slug, description string) error {
	args := []string{"repo", "create", slug, "--public"}
	if description != "" {
		args = append(args, "--description", description)
	}
	_, err := sh.MustRun("", "gh", args...)
	if err != nil {
		return fmt.Errorf("gh repo create %s: %w", slug, err)
	}
	return nil
}

// ListSlices returns all issues with the given label (default "slice"),
// in both open and closed states, up to 200.
func ListSlices(label string) ([]Issue, error) {
	if label == "" {
		label = "slice"
	}
	EnsureLabel(label)
	r, err := sh.MustRun("", "gh", "issue", "list",
		"--label", label,
		"--state", "all",
		"--json", "number,title,body,state,labels",
		"--limit", "200",
	)
	if err != nil {
		return nil, err
	}
	var issues []Issue
	if err := json.Unmarshal([]byte(r.Stdout), &issues); err != nil {
		return nil, fmt.Errorf("decode gh issue list: %w", err)
	}
	return issues, nil
}

// CreateIssue opens a new issue with the given labels. Returns its number.
// Missing labels on the repo are created (idempotent — "already exists"
// is ignored).
func CreateIssue(title, body string, labels []string) (int, error) {
	for _, l := range labels {
		EnsureLabel(l)
	}
	args := []string{"issue", "create", "--title", title, "--body", body}
	for _, l := range labels {
		args = append(args, "--label", l)
	}
	r, err := sh.MustRun("", "gh", args...)
	if err != nil {
		return 0, err
	}
	url := strings.TrimSpace(r.Stdout)
	// gh may print "Creating issue in ..." lines; the URL is on the last line.
	if lines := strings.Split(url, "\n"); len(lines) > 0 {
		url = strings.TrimSpace(lines[len(lines)-1])
	}
	slash := strings.LastIndex(url, "/")
	if slash < 0 {
		return 0, fmt.Errorf("could not parse issue URL from gh output: %q", url)
	}
	var n int
	if _, err := fmt.Sscanf(url[slash+1:], "%d", &n); err != nil {
		return 0, fmt.Errorf("parse issue number from %q: %w", url, err)
	}
	return n, nil
}

// GetIssue fetches one issue with its labels + body.
func GetIssue(num int) (*Issue, error) {
	r, err := sh.MustRun("", "gh", "issue", "view", fmt.Sprintf("%d", num),
		"--json", "number,title,body,state,labels")
	if err != nil {
		return nil, err
	}
	var i Issue
	if err := json.Unmarshal([]byte(r.Stdout), &i); err != nil {
		return nil, fmt.Errorf("decode gh issue view: %w", err)
	}
	return &i, nil
}

// CurrentAttempt extracts N from any `<prefix>N` label on the issue.
// Returns 0 if none is set.
func (i Issue) CurrentAttempt(prefix string) int {
	for _, l := range i.Labels {
		if strings.HasPrefix(l.Name, prefix) {
			var n int
			if _, err := fmt.Sscanf(l.Name[len(prefix):], "%d", &n); err == nil {
				return n
			}
		}
	}
	return 0
}

// SetAttemptLabel removes any existing attempt:N label and applies the
// new one. Creates the label on the repo first if it doesn't exist.
func SetAttemptLabel(num int, prefix string, n int) error {
	current, err := GetIssue(num)
	if err != nil {
		return err
	}
	// Remove existing attempt:* labels.
	for _, l := range current.Labels {
		if strings.HasPrefix(l.Name, prefix) {
			if _, err := sh.Run("", "gh", "issue", "edit", fmt.Sprintf("%d", num),
				"--remove-label", l.Name); err != nil {
				return err
			}
		}
	}
	newLabel := fmt.Sprintf("%s%d", prefix, n)
	// Create the label if missing (idempotent — ignore "already exists").
	_, _ = sh.Run("", "gh", "label", "create", newLabel, "--description", "attempt counter", "--color", "ededed")
	r, err := sh.Run("", "gh", "issue", "edit", fmt.Sprintf("%d", num), "--add-label", newLabel)
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return fmt.Errorf("gh issue edit --add-label %s: %s", newLabel, r.Combined())
	}
	return nil
}

// CloseIssue closes the issue with an optional comment.
func CloseIssue(num int, comment string) error {
	args := []string{"issue", "close", fmt.Sprintf("%d", num)}
	if comment != "" {
		args = append(args, "--comment", comment)
	}
	r, err := sh.MustRun("", "gh", args...)
	if err != nil {
		return err
	}
	_ = r
	return nil
}

// CommentIssue posts a comment on an open issue. Used when an attempt
// fails to attach the failure context for the next code agent.
func CommentIssue(num int, body string) error {
	_, err := sh.MustRun("", "gh", "issue", "comment", fmt.Sprintf("%d", num), "--body", body)
	return err
}

// OldestOpenSlice returns the oldest open issue worth working on.
// It tries label preferenceOrder in order — the first non-empty
// result wins. This is how refactor:thermonuclear issues queue ahead
// of feature slices.
//
// If `labels` is empty, defaults to ["refactor:thermonuclear", "slice"].
func OldestOpenSlice(labels ...string) (*Issue, error) {
	if len(labels) == 0 {
		labels = []string{"refactor:thermonuclear", "slice"}
	}
	for _, lbl := range labels {
		if lbl == "" {
			continue
		}
		EnsureLabel(lbl)
		r, err := sh.MustRun("", "gh", "issue", "list",
			"--label", lbl,
			"--state", "open",
			"--json", "number,title,body,state,labels",
			"--limit", "200",
		)
		if err != nil {
			return nil, err
		}
		var issues []Issue
		if err := json.Unmarshal([]byte(r.Stdout), &issues); err != nil {
			return nil, fmt.Errorf("decode gh issue list: %w", err)
		}
		if len(issues) == 0 {
			continue
		}
		oldest := &issues[0]
		for i := range issues {
			if issues[i].Number < oldest.Number {
				oldest = &issues[i]
			}
		}
		return oldest, nil
	}
	return nil, nil
}

// HandoffForReview is the human-review escalation called at attempt cap.
// It adds the HITL label, comments with the latest failure, and then
// CLOSES the issue so it leaves the open-slice queue. Leaving it open
// wedged the loop: OldestOpenSlice kept re-selecting the capped issue
// and Work re-escalated it every iteration forever. Closing matches the
// documented orchestrator contract (PRD §9); a human finds escalations
// via `is:closed label:<hitl>` and re-opens after addressing.
func HandoffForReview(issue int, hitlLabel, reason string) error {
	// Create the label idempotently.
	_, _ = sh.Run("", "gh", "label", "create", hitlLabel,
		"--description", "needs human review",
		"--color", "d73a4a")
	if _, err := sh.Run("", "gh", "issue", "edit", fmt.Sprintf("%d", issue), "--add-label", hitlLabel); err != nil {
		return err
	}
	body := fmt.Sprintf("Hit attempt cap — escalating for human review and closing so the loop can proceed. Re-open after addressing.\n\nLast failure:\n```\n%s\n```", reason)
	if err := CommentIssue(issue, body); err != nil {
		return err
	}
	// Close without a second comment (the detail is in the comment above).
	return CloseIssue(issue, "")
}
