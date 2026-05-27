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
func CreateIssue(title, body string, labels []string) (int, error) {
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

// BumpAttemptLabel removes attempt:N and adds attempt:N+1, returning the
// new attempt number. If no attempt label is set, starts at 1.
//
// TODO(007-builder): implement via two `gh issue edit --remove-label
// / --add-label` calls. Deferred until loop.Work needs it.
func BumpAttemptLabel(issue int, prefix string) (int, error) {
	return 0, fmt.Errorf("TODO: BumpAttemptLabel not yet implemented")
}

// HandoffForReview opens a PR with `awaiting-human-review` and closes
// the underlying issue (per the HITL pattern in lessons §2).
//
// TODO(007-builder): implement. Deferred until loop.Work needs it.
func HandoffForReview(issue int, branch, hitlLabel string) error {
	return fmt.Errorf("TODO: HandoffForReview not yet implemented")
}
