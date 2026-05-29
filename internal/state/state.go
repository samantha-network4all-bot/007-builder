// Package state reads and writes the orchestrator's per-project status
// file (.slate/state.json by default for slate; configurable via
// paths.state). JSON, not bash-scraped — see lessons §2.3 / §2.6.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// State is the JSON document persisted between subcommand runs.
type State struct {
	UpdatedAt    string `json:"updatedAt"`
	CurrentIssue int    `json:"currentIssue,omitempty"`
	Attempt      int    `json:"attempt,omitempty"`
	NextAction   string `json:"nextAction,omitempty"`  // "code", "check", "abort", "done"
	LastOutcome  string `json:"lastOutcome,omitempty"` // llm.Outcome.String() or "feature-test-failed" etc.
	LastCommit   string `json:"lastCommit,omitempty"`
	LastError    string `json:"lastError,omitempty"`

	// ConsecutiveGreen counts successfully-closed feature slices since
	// the last reset (failure OR thermo-nuclear review). When it hits
	// the review window size (default 5) the orchestrator fires
	// review.Run and resets to 0.
	ConsecutiveGreen int `json:"consecutiveGreen,omitempty"`

	// ReviewWindowBaseSHA is the commit immediately before the current
	// window of green slices. Used as the base for `git diff base..HEAD`
	// when the review fires. Updated to HEAD on every reset.
	ReviewWindowBaseSHA string `json:"reviewWindowBaseSHA,omitempty"`

	// ReviewWindowSlices accumulates the slice issue numbers + titles
	// closed since the last review. Embedded in the thermo prompt so
	// the model knows which slices it's reviewing.
	ReviewWindowSlices []ClosedSlice `json:"reviewWindowSlices,omitempty"`
}

// ClosedSlice is a compact record of a successfully-closed slice.
type ClosedSlice struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
}

// Load returns the persisted state from dir/state.json. A missing file
// is not an error — it returns a zero State.
func Load(dir string) (*State, error) {
	p := filepath.Join(dir, "state.json")
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return &State{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("decode %s: %w", p, err)
	}
	return &s, nil
}

// Save writes the state to dir/state.json atomically (tempfile + rename).
func Save(dir string, s *State) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	s.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')

	final := filepath.Join(dir, "state.json")
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}
