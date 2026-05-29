// Package loop owns the high-level orchestrator commands: bootstrap,
// work, and run (the full iterative loop). The per-issue state machine
// also lives here.
package loop

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/samantha-network4all-bot/007-builder/internal/checks"
	"github.com/samantha-network4all-bot/007-builder/internal/config"
	"github.com/samantha-network4all-bot/007-builder/internal/github"
	"github.com/samantha-network4all-bot/007-builder/internal/llm"
	"github.com/samantha-network4all-bot/007-builder/internal/plan"
	"github.com/samantha-network4all-bot/007-builder/internal/repourl"
	"github.com/samantha-network4all-bot/007-builder/internal/review"
	"github.com/samantha-network4all-bot/007-builder/internal/sh"
	"github.com/samantha-network4all-bot/007-builder/internal/state"
	"github.com/samantha-network4all-bot/007-builder/internal/stream"
	"github.com/samantha-network4all-bot/007-builder/internal/tmpl"
	"github.com/samantha-network4all-bot/007-builder/internal/ui"
)

// Bootstrap is a one-shot setup:
//   1. Load .agent/config.yaml.
//   2. Verify gh CLI is authenticated.
//   3. Create the GitHub repo if it does not already exist.
//   4. `git init` here if missing; add `origin` remote.
//   5. Commit whatever is currently in the working tree (typically
//      PRD.md, lessons-learned.md, README.md, .agent/...) and push.
//
// Bootstrap intentionally does NOT write any project-specific seed
// files. The first `next-issue` call generates an S1 issue that
// instructs the code agent to scaffold the application from PRD §3.
// This keeps 007-builder agnostic of Swift/Go/whatever.
func Bootstrap(args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "print actions without executing")
	configPath := fs.String("config", "", "path to .agent/config.yaml (default: ./.agent/config.yaml)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}

	cp := *configPath
	if cp == "" {
		cp = cwd
	}
	cfg, err := config.Load(cp)
	if err != nil {
		return err
	}
	if err := cfg.Validate(config.RequireProjectRepo, config.RequireProjectName); err != nil {
		return err
	}

	ui.Header("bootstrap")
	ui.KV("project", cfg.ProjectName)
	ui.KV("repo", cfg.ProjectRepo)
	ui.KV("cwd", cwd)

	if *dryRun {
		fmt.Println("would: gh auth status")
		fmt.Printf("would: ensure repo %s exists (create if missing)\n", cfg.ProjectRepo)
		fmt.Printf("would: git init (if missing) + add remote origin https://github.com/%s.git\n", cfg.ProjectRepo)
		fmt.Println("would: git add -A && git commit && git push -u origin main")
		return nil
	}

	if err := github.RequireAuth(); err != nil {
		return err
	}

	exists, err := github.RepoExists(cfg.ProjectRepo)
	if err != nil {
		return err
	}
	if !exists {
		desc := fmt.Sprintf("%s — agent-built via 007-builder", cfg.ProjectName)
		if err := github.CreateRepo(cfg.ProjectRepo, desc); err != nil {
			return err
		}
		ui.OK("created repo https://github.com/%s", cfg.ProjectRepo)
	} else {
		ui.Note("repo already exists: https://github.com/%s", cfg.ProjectRepo)
	}

	if err := ensureLocalGit(cwd, cfg.ProjectRepo); err != nil {
		return err
	}

	if err := initialCommitAndPush(cwd, cfg.ProjectName); err != nil {
		return err
	}

	ui.OK("bootstrap complete")
	return nil
}

// ensureLocalGit makes sure cwd is a git repo on `main` with `origin`
// pointing at the GitHub repo. Safe to re-run.
func ensureLocalGit(dir, repoSlug string) error {
	gitDir := filepath.Join(dir, ".git")
	if _, err := os.Stat(gitDir); os.IsNotExist(err) {
		if _, err := sh.MustRun(dir, "git", "init", "-q"); err != nil {
			return err
		}
	}

	// Ensure branch is `main`. `git branch -M main` works on an
	// unborn branch too.
	if r, _ := sh.Run(dir, "git", "rev-parse", "--abbrev-ref", "HEAD"); strings.TrimSpace(r.Stdout) != "main" {
		if _, err := sh.MustRun(dir, "git", "branch", "-M", "main"); err != nil {
			return err
		}
	}

	// Ensure `origin` remote points where we want.
	wantURL := fmt.Sprintf("https://github.com/%s.git", repoSlug)
	r, _ := sh.Run(dir, "git", "remote", "get-url", "origin")
	switch {
	case r.ExitCode != 0:
		if _, err := sh.MustRun(dir, "git", "remote", "add", "origin", wantURL); err != nil {
			return err
		}
	case strings.TrimSpace(r.Stdout) != wantURL:
		if _, err := sh.MustRun(dir, "git", "remote", "set-url", "origin", wantURL); err != nil {
			return err
		}
	}
	return nil
}

// initialCommitAndPush stages everything in cwd, commits with a seed
// message, and pushes. If the tree is clean it just pushes the current
// HEAD.
func initialCommitAndPush(dir, projectName string) error {
	r, err := sh.MustRun(dir, "git", "status", "--porcelain")
	if err != nil {
		return err
	}
	dirty := strings.TrimSpace(r.Stdout) != ""

	if dirty {
		if _, err := sh.MustRun(dir, "git", "add", "-A"); err != nil {
			return err
		}
		msg := fmt.Sprintf("seed: %s scaffold (PRD + lessons + .agent config)\n\nCreated by 007-builder bootstrap.", projectName)
		if _, err := sh.MustRun(dir, "git", "commit", "-m", msg); err != nil {
			return err
		}
		ui.OK("created seed commit")
	} else {
		ui.Note("working tree clean — nothing new to commit")
	}

	push, err := sh.Run(dir, "git", "push", "-u", "origin", "main")
	if err != nil {
		return err
	}
	if push.ExitCode != 0 {
		return fmt.Errorf("git push failed:\n%s", push.Combined())
	}
	ui.OK("pushed to origin/main")
	return nil
}

// ensureCleanTree makes the working tree clean so Work can start. A
// dirty tree at this point is always detritus from an interrupted or
// failed previous attempt (a code agent edited files but didn't commit,
// e.g. because the build broke or it errored out). main is the source of
// truth and the next attempt re-derives from the issue + PreviousFailure,
// so by default we discard the leftover changes and proceed — otherwise
// the loop re-detects the same dirty tree on every iteration and spins
// forever. With loop.refuse_dirty_tree set, we instead return an error
// (the old hard-stop). Safe to call on a clean tree (no-op).
func ensureCleanTree(dir string, cfg *config.Config) error {
	r, _ := sh.Run(dir, "git", "status", "--porcelain")
	dirty := strings.TrimSpace(r.Stdout)
	if dirty == "" {
		return nil
	}
	if cfg.RefuseDirtyTree {
		return fmt.Errorf("working tree is dirty; refusing to start work (loop.refuse_dirty_tree):\n%s", dirty)
	}
	ui.Warn("working tree dirty — discarding leftover changes from a prior run:\n%s", dirty)
	if _, err := sh.MustRun(dir, "git", "reset", "--hard", "HEAD"); err != nil {
		return fmt.Errorf("git reset --hard: %w", err)
	}
	// -d removes untracked dirs/files; ignored paths (build/, the state
	// dir, *.xcodeproj) are preserved because we omit -x.
	if _, err := sh.MustRun(dir, "git", "clean", "-fd"); err != nil {
		return fmt.Errorf("git clean -fd: %w", err)
	}
	ui.OK("working tree reset to HEAD; continuing")
	return nil
}

// ReviewWindowSize is how many consecutive green slices trigger a
// thermo-nuclear review. The counter resets on any failure.
const ReviewWindowSize = 5

// StallThreshold is how many times the same issue can produce the
// same outcome before the orchestrator declares a loop-stall and
// closes the issue as "already implemented" rather than cycling.
const StallThreshold = 3

// minNonEmptyDiffLines is the minimum number of changed lines (in
// git diff --stat) for a code-agent run to be considered "did real
// work". Below this the run is treated as a no-op (agent found
// nothing to change) and the issue is a candidate for auto-close.
const minNonEmptyDiffLines = 3

// Work picks the oldest open slice issue, invokes the code agent,
// runs the feature check, and either closes the issue (green) or
// bumps attempt:N (red, with N<cap). At N==cap it labels for HITL
// review and leaves the issue open.
//
//	builder work [--issue N]    Pin to a specific issue.
//	builder work [--dry-run]    Render the prompt; don't invoke the LLM.
func Work(args []string) error {
	fs := flag.NewFlagSet("work", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to .agent/config.yaml")
	issueOverride := fs.Int("issue", 0, "force a specific issue number")
	dryRun := fs.Bool("dry-run", false, "render the prompt; do not invoke the LLM")
	caveman := fs.Bool("caveman", false, "terse mode: prepend a 'no monologues' directive to the prompt")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	cp := *cfgPath
	if cp == "" {
		cp = cwd
	}
	cfg, err := config.Load(cp)
	if err != nil {
		return err
	}
	if err := cfg.Validate(config.RequireProjectRepo, config.RequirePRDPath, config.RequirePromptsDir, config.RequireFeatureBinary); err != nil {
		return err
	}

	// Recover from a dirty tree. Uncommitted changes here are always
	// detritus from an interrupted or failed previous attempt — main is
	// the source of truth, and the next attempt re-derives from the
	// issue + PreviousFailure. So by default we discard them and proceed
	// rather than wedging the loop (which otherwise re-detects the dirty
	// tree every iteration forever). Set loop.refuse_dirty_tree: true to
	// restore the old hard-stop.
	if err := ensureCleanTree(cwd, cfg); err != nil {
		return err
	}

	// Pick issue.
	var issue *github.Issue
	if *issueOverride > 0 {
		issue, err = github.GetIssue(*issueOverride)
		if err != nil {
			return err
		}
	} else {
		// refactor:thermonuclear issues come first; then slice.
		issue, err = github.OldestOpenSlice(review.RefactorLabel, cfg.SliceLabel)
		if err != nil {
			return err
		}
		if issue == nil {
			ui.Note("no open slice issues — nothing to do")
			return nil
		}
	}

	attempt := issue.CurrentAttempt(cfg.AttemptLabelPrefix)
	ui.Header("work")
	ui.Issue(issue.Number, issue.Title)
	ui.KV("attempt", fmt.Sprintf("%d / %d", attempt, cfg.AttemptsPerIssue))

	if attempt >= cfg.AttemptsPerIssue {
		ui.Warn("attempt cap already reached — escalating for HITL")
		return github.HandoffForReview(issue.Number, cfg.HITLLabel,
			"attempt cap reached before this run started")
	}

	// Stall detection: if this issue already produced the same
	// outcome (attempt-1) times in a row, close it as "already
	// implemented" instead of burning another iteration.
	if err := checkLoopStall(cwd, cfg, issue.Number, attempt); err != nil {
		return err
	}

	// Render the code prompt.
	tmplPath := filepath.Join(cwd, cfg.PromptsDir, "PROMPT-code.tmpl")
	acceptance := extractAcceptanceBlock(issue.Body)
	tmpPrompt, err := tmpl.RenderToTemp("builder-prompt-", tmplPath, map[string]any{
		"IssueNumber":     issue.Number,
		"IssueTitle":      issue.Title,
		"IssueBody":       issue.Body,
		"AcceptanceJSON":  acceptance,
		"PreviousFailure": readPreviousFailure(cwd, cfg),
	})
	if err != nil {
		return err
	}
	defer os.Remove(tmpPrompt)

	if *dryRun {
		ui.Header("rendered code prompt")
		if b, err := os.ReadFile(tmpPrompt); err == nil {
			fmt.Println(string(b))
		}
		return nil
	}

	ui.Step("invoking code agent (%s %s)", cfg.LLMCLI, cfg.LLMModel)
	// Invoke the code agent.
	// Streaming sink renders pi's live events (tool calls + char counter)
	// so the user sees progress instead of waiting in silence for what
	// can be 5-10 minutes of agent work.
	sink := stream.NewEventSink(false)
	inv := llm.Invocation{
		CLI:              cfg.LLMCLI,
		Model:            cfg.LLMModel,
		Thinking:         cfg.LLMThinking,
		Mode:             "json",
		SystemPromptFile: tmpPrompt,
		UserMessage: fmt.Sprintf("Implement issue #%d. Commit on main and push when done.",
			issue.Number),
		Tools:       cfg.LLMTools,
		WorkingDir:  cwd,
		TrackCommit: true,
		Caveman:     *caveman || cfg.LLMCaveman,
		Skills:      cfg.ResolveCodeSkills(cwd),
		Stream:      sink,
	}
	res, err := llm.Run(inv)
	sink.Finish()
	if err != nil {
		return fmt.Errorf("invoke code agent: %w", err)
	}
	ui.Outcome(res.Outcome.String(),
		fmt.Sprintf("(exit=%d, HEAD %s → %s)", res.ExitCode, abbrev(res.HEADBefore), abbrev(res.HEADAfter)))

	// Persist state.
	st := &state.State{
		CurrentIssue: issue.Number,
		Attempt:      attempt,
		LastOutcome:  res.Outcome.String(),
		LastCommit:   res.HEADAfter,
	}
	_ = state.Save(filepath.Join(cwd, cfg.StateDir), st)

	if res.Outcome != llm.OutcomeSucceeded {
		resetGreenStreak(cwd, cfg)
		newAttempt := attempt + 1
		_ = github.SetAttemptLabel(issue.Number, cfg.AttemptLabelPrefix, newAttempt)
		comment := fmt.Sprintf("Attempt %d failed at code phase (outcome=%s).\n\nstderr:\n```\n%s\n```",
			newAttempt, res.Outcome, truncate(res.Stderr, 4000))
		_ = github.CommentIssue(issue.Number, comment)
		if newAttempt >= cfg.AttemptsPerIssue {
			return github.HandoffForReview(issue.Number, cfg.HITLLabel,
				fmt.Sprintf("outcome=%s after %d attempts", res.Outcome, newAttempt))
		}
		return fmt.Errorf("code agent outcome=%s; bumped to attempt %d", res.Outcome, newAttempt)
	}

	// Diff-empty check: if the code agent touched almost nothing
	// the issue is likely already implemented. Close it immediately
	// and let the planner pick the next real slice.
	if isDiff, empty := isDiffEmpty(cwd, cfg); isDiff && empty {
		ui.Warn("code agent produced near-empty diff — issue already implemented, auto-closing")
		_ = github.CommentIssue(issue.Number,
			"Auto-closed by 007-builder: code agent produced no meaningful changes. Issue appears already implemented.")
		if err := github.CloseIssue(issue.Number,
			"Closed by 007-builder: diff empty, issue already implemented."); err != nil {
			return err
		}
		resetGreenStreak(cwd, cfg)
		resetLoopStall(cwd, cfg)
		return nil
	}

	// Code agent committed something. Run feature check.
	featureErr := runFeatureCheck(cwd, cfg, acceptance, issue.Number, attempt)

	// Build the per-attempt comment. Screenshot is best-effort — if it's
	// missing (e.g. the app doesn't expose /screenshot yet) we still
	// post the textual outcome so the issue page reflects progress.
	verdict := "pass"
	summary := "feature check green"
	if featureErr != nil {
		verdict = "fail"
		summary = truncate(featureErr.Error(), 2000)
	}

	shotPath := readScreenshotPath(cwd, cfg)
	imageLine := "_(no screenshot — `/screenshot` endpoint not yet implemented)_"
	if shotPath != "" {
		if err := commitAndPushScreenshot(cwd, shotPath, issue.Number, attempt, verdict); err != nil {
			ui.Warn("screenshot commit: %v", err)
			imageLine = "_(screenshot captured but push failed: " + err.Error() + ")_"
		} else {
			rawURL := repourl.Raw(cfg.ProjectRepo, "main", shotPath)
			imageLine = fmt.Sprintf("![Slate after attempt %d](%s)", attempt, rawURL)
		}
	}
	body := fmt.Sprintf("## Attempt %d — %s\n\n%s\n\n%s",
		attempt, verdict, imageLine, summary)
	if err := github.CommentIssue(issue.Number, body); err != nil {
		ui.Warn("comment issue: %v", err)
	} else {
		ui.OK("posted attempt %d outcome to #%d", attempt, issue.Number)
	}

	if featureErr != nil {
		resetGreenStreak(cwd, cfg)
		newAttempt := attempt + 1
		_ = github.SetAttemptLabel(issue.Number, cfg.AttemptLabelPrefix, newAttempt)
		if newAttempt >= cfg.AttemptsPerIssue {
			return github.HandoffForReview(issue.Number, cfg.HITLLabel,
				fmt.Sprintf("feature check failed after %d attempts", newAttempt))
		}
		return fmt.Errorf("feature check failed; bumped to attempt %d", newAttempt)
	}

	// Track outcome for stall detection. If the same issue keeps
	// getting re-opened and passing, Count climbs toward threshold.
	outcome := "pass"
	if featureErr != nil {
		outcome = "fail"
	}
	recordLoopOutcome(cwd, cfg, issue.Number, outcome)

	// Green. Close the issue and update the consecutive-green streak.
	if err := github.CloseIssue(issue.Number,
		fmt.Sprintf("Closed by 007-builder. Commit %s passed the feature check.", abbrev(res.HEADAfter))); err != nil {
		return err
	}
	ui.OK("closed issue #%d", issue.Number)
	resetLoopStall(cwd, cfg)

	// Only feature slices count toward the thermo window. Refactor
	// slices opened by the review itself don't trigger another review.
	if issue.HasLabel(cfg.SliceLabel) {
		bumpGreenStreak(cwd, cfg, issue.Number, issue.Title, res.HEADAfter)
		maybeFireThermoReview(cwd, cfg)
	}
	return nil
}

// bumpGreenStreak loads state, increments ConsecutiveGreen, appends
// this slice to ReviewWindowSlices, and saves. ReviewWindowBaseSHA is
// captured at the start of a fresh streak (when counter goes 0 → 1).
func bumpGreenStreak(cwd string, cfg *config.Config, num int, title, headSHA string) {
	st, _ := state.Load(filepath.Join(cwd, cfg.StateDir))
	if st.ConsecutiveGreen == 0 {
		// Start of a new window — the base is the commit BEFORE the
		// just-closed slice's commit chain. Cheapest correct answer:
		// resolve HEAD~1 right now (head of repo after this slice's
		// commits, minus one).
		if base, ok := resolveHEADParent(cwd, headSHA); ok {
			st.ReviewWindowBaseSHA = base
		}
		st.ReviewWindowSlices = nil
	}
	st.ConsecutiveGreen++
	st.ReviewWindowSlices = append(st.ReviewWindowSlices, state.ClosedSlice{Number: num, Title: title})
	_ = state.Save(filepath.Join(cwd, cfg.StateDir), st)
	ui.Note("consecutive green slices: %d/%d", st.ConsecutiveGreen, ReviewWindowSize)
}

func resetGreenStreak(cwd string, cfg *config.Config) {
	st, _ := state.Load(filepath.Join(cwd, cfg.StateDir))
	if st.ConsecutiveGreen == 0 && st.ReviewWindowBaseSHA == "" {
		return // already clean
	}
	st.ConsecutiveGreen = 0
	st.ReviewWindowBaseSHA = ""
	st.ReviewWindowSlices = nil
	_ = state.Save(filepath.Join(cwd, cfg.StateDir), st)
}

// maybeFireThermoReview triggers review.Run when the counter hits the
// window size. On any review outcome (clean OR blockers opened) the
// counter resets to 0 — the window is closed.
func maybeFireThermoReview(cwd string, cfg *config.Config) {
	st, _ := state.Load(filepath.Join(cwd, cfg.StateDir))
	if st.ConsecutiveGreen < ReviewWindowSize {
		return
	}
	// Convert state slices to github.Issue shape the reviewer expects.
	slices := make([]github.Issue, 0, len(st.ReviewWindowSlices))
	for _, s := range st.ReviewWindowSlices {
		slices = append(slices, github.Issue{Number: s.Number, Title: s.Title})
	}
	opened, err := review.Run(cwd, cfg, st.ReviewWindowBaseSHA, slices)
	if err != nil {
		ui.Warn("thermo review failed: %v", err)
	} else if opened > 0 {
		ui.Warn("thermo opened %d refactor issue(s); loop will pick them up first", opened)
	}
	// Whatever happened, the window closes.
	resetGreenStreak(cwd, cfg)
}

// resolveHEADParent returns the parent commit of sha. Empty + false on
// failure (e.g. initial commit has no parent).
func resolveHEADParent(cwd, sha string) (string, bool) {
	if sha == "" {
		sha = "HEAD"
	}
	r, err := sh.Run(cwd, "git", "rev-parse", sha+"^")
	if err != nil || r.ExitCode != 0 {
		return "", false
	}
	return strings.TrimSpace(r.Stdout), true
}

// Run repeats next-issue + work until next-issue says "PRD complete"
// or until an iteration errors.
//
//	builder loop [--max N]    Cap total iterations (default unlimited).
func Run(args []string) error {
	fs := flag.NewFlagSet("loop", flag.ContinueOnError)
	maxIter := fs.Int("max", 0, "max iterations (0 = unlimited)")
	caveman := fs.Bool("caveman", false, "terse mode: prepend a 'no monologues' directive to every agent call")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var inner []string
	if *caveman {
		inner = []string{"--caveman"}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	cfg, err := config.Load(cwd)
	if err != nil {
		return err
	}

	i := 0
	for {
		if *maxIter > 0 && i >= *maxIter {
			ui.Note("reached --max %d, stopping", *maxIter)
			return nil
		}
		i++
		ui.Header("iter %d", i)

		// Only open a new issue when nothing's open. Otherwise keep
		// hammering the current oldest open slice until it closes or
		// hits the attempt cap.
		// Refactor:thermonuclear issues queue ahead of feature slices.
		open, err := github.OldestOpenSlice(review.RefactorLabel, cfg.SliceLabel)
		if err != nil {
			return fmt.Errorf("iter %d: list open issues: %w", i, err)
		}
		if open == nil {
			ui.Note("no open slice or refactor issue — asking planner for a new one")
			if err := plan.NextIssue(inner); err != nil {
				return fmt.Errorf("iter %d: next-issue: %w", i, err)
			}
		} else {
			ui.Note("continuing on open issue #%d (%s)", open.Number, firstLabel(open))
		}

		if err := Work(inner); err != nil {
			if strings.Contains(err.Error(), "__stall__") {
				ui.Note("iter %d: %v — continuing to next issue", i, err)
			} else {
				ui.Fail("iter %d: work returned: %v", i, err)
				// continue — failed iterations bump attempt; loop will pick it back up.
			}
		}
	}
}

// Helpers.

func extractAcceptanceBlock(body string) string {
	// Find the first ```json``` fenced block and return its contents.
	const fence = "```json"
	start := strings.Index(body, fence)
	if start < 0 {
		return "{}"
	}
	rest := body[start+len(fence):]
	end := strings.Index(rest, "```")
	if end < 0 {
		return "{}"
	}
	return strings.TrimSpace(rest[:end])
}

func readPreviousFailure(cwd string, cfg *config.Config) string {
	stateDir := cfg.StateDir
	if stateDir == "" {
		stateDir = ".slate"
	}
	b, err := os.ReadFile(filepath.Join(cwd, stateDir, "checks", "feature.json"))
	if err != nil {
		return ""
	}
	return string(b)
}

// firstLabel returns the first label name on an issue, for log context.
func firstLabel(i *github.Issue) string {
	for _, l := range i.Labels {
		return l.Name
	}
	return "unlabeled"
}

func abbrev(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(truncated)"
}

// runFeatureCheck parses the issue's acceptance JSON and invokes
// checks.RunFeature in-process. Previously this re-exec'd the builder
// binary via os.Executable() — flagged by the thermo-nuclear sweep as
// a gratuitous process hop. checks is a sibling package; we can call
// it directly.
func runFeatureCheck(cwd string, cfg *config.Config, acceptanceJSON string, issueNum, attempt int) error {
	var acc checks.Acceptance
	if strings.TrimSpace(acceptanceJSON) != "" {
		if err := json.Unmarshal([]byte(acceptanceJSON), &acc); err != nil {
			return fmt.Errorf("parse acceptance JSON: %w", err)
		}
	}
	_, err := checks.RunFeature(cwd, cfg, checks.FeatureOptions{
		Probes:  acc.Acceptance,
		Issue:   issueNum,
		Attempt: attempt,
	})
	return err
}

// readScreenshotPath pulls the ScreenshotPath field out of the most
// recent feature.json report. Returns "" if missing.
func readScreenshotPath(cwd string, cfg *config.Config) string {
	stateDir := cfg.StateDir
	if stateDir == "" {
		stateDir = ".slate"
	}
	b, err := os.ReadFile(filepath.Join(cwd, stateDir, "checks", "feature.json"))
	if err != nil {
		return ""
	}
	var report struct {
		ScreenshotPath string `json:"screenshotPath"`
	}
	if err := json.Unmarshal(b, &report); err != nil {
		return ""
	}
	return report.ScreenshotPath
}

// commitAndPushScreenshot stages, commits, and pushes a single PNG.
// Uses --allow-empty so a duplicate (unchanged) PNG doesn't fail the
// commit step.
func commitAndPushScreenshot(cwd, relPath string, issue, attempt int, verdict string) error {
	add, err := sh.Run(cwd, "git", "add", relPath)
	if err != nil {
		return err
	}
	if add.ExitCode != 0 {
		return fmt.Errorf("git add %s: %s", relPath, add.Combined())
	}
	msg := fmt.Sprintf("screenshots: S%d attempt %d (%s)", issue, attempt, verdict)
	commit, err := sh.Run(cwd, "git", "commit", "-m", msg)
	if err != nil {
		return err
	}
	// "nothing to commit" is fine — same screenshot from a re-run.
	if commit.ExitCode != 0 && !strings.Contains(commit.Combined(), "nothing to commit") {
		return fmt.Errorf("git commit: %s", commit.Combined())
	}
	push, err := sh.Run(cwd, "git", "push", "origin", "main")
	if err != nil {
		return err
	}
	if push.ExitCode != 0 {
		return fmt.Errorf("git push: %s", push.Combined())
	}
	return nil
}

// ─── Stall detection helpers ──────────────────────────────────────

// isDiffEmpty returns (true, true) when the working tree has a
// commit HEAD whose diff against HEAD~1 contains fewer than
// minNonEmptyDiffLines changed lines. Returns (false, false) when
// there is no prior commit to diff against (single-commit repo) or
// when git diff fails — the caller should treat that as "not empty".
func isDiffEmpty(cwd string, cfg *config.Config) (isDiff bool, empty bool) {
	// Check we have at least 2 commits.
	r, err := sh.Run(cwd, "git", "rev-list", "--count", "HEAD")
	if err != nil || r.ExitCode != 0 {
		return false, false
	}
	var count int
	if _, err := fmt.Sscanf(strings.TrimSpace(r.Stdout), "%d", &count); err != nil || count < 2 {
		return false, false
	}

	isDiff = true
	r, err = sh.Run(cwd, "git", "diff", "--stat", "HEAD~1", "HEAD")
	if err != nil || r.ExitCode != 0 {
		return isDiff, false
	}
	stat := strings.TrimSpace(r.Stdout)
	if stat == "" {
		return isDiff, true
	}
	// Count changed lines by summing the last field of each
	// "file | N +-" line. Quick heuristic: count '+'-only tokens.
	// Even simpler: count the number of lines that mention
	// insertions/deletions.
	lines := strings.Split(stat, "\n")
	changed := 0
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		// Format: "path/to/file | 5 +++--" or "5 files changed, 10 insertions(+), 2 deletions(-)"
		if strings.Contains(l, "insertion") || strings.Contains(l, "deletion") {
			// summary line — parse the numbers
			for _, field := range strings.Split(l, ",") {
				field = strings.TrimSpace(field)
				var n int
				if _, err := fmt.Sscanf(field, "%d", &n); err == nil {
					changed += n
				}
			}
			continue
		}
		// per-file line: extract the change count before the +-
		if idx := strings.Index(l, "|"); idx >= 0 {
			right := strings.TrimSpace(l[idx+1:])
			var n int
			if _, err := fmt.Sscanf(right, "%d", &n); err == nil {
				changed += n
			}
		}
	}
	return isDiff, changed < minNonEmptyDiffLines
}

// checkLoopStall reads the LoopStall from state and checks whether
// the current (issue, attempt) would exceed the threshold. If so it
// auto-closes the issue and returns an error that stops Work.
func checkLoopStall(cwd string, cfg *config.Config, issueNum, attempt int) error {
	st, _ := state.Load(filepath.Join(cwd, cfg.StateDir))
	// No prior stall record or different issue — not stalled yet.
	if st.LoopStall.IssueNumber != issueNum {
		return nil
	}
	if st.LoopStall.Count >= StallThreshold {
		ui.Warn("loop stall detected: issue #%d produced outcome %q %d times — auto-closing",
			issueNum, st.LoopStall.Outcome, st.LoopStall.Count)
		_ = github.CommentIssue(issueNum,
			fmt.Sprintf("Auto-closed by 007-builder: loop-stall detected.\n\n"+
				"This issue was auto-closed after producing the same outcome (%s) %d times in a row.\n"+
				"If this is incorrect, re-open and re-label.",
				st.LoopStall.Outcome, st.LoopStall.Count))
		if err := github.CloseIssue(issueNum,
			fmt.Sprintf("Closed by 007-builder: loop-stall (outcome=%s x%d)",
				st.LoopStall.Outcome, st.LoopStall.Count)); err != nil {
			return err
		}
		resetGreenStreak(cwd, cfg)
		resetLoopStall(cwd, cfg)
		// Return a non-error sentinel so the loop continues to the next
		// issue instead of halting the whole Run iteration.
		return fmt.Errorf("__stall__ issue #%d auto-closed (outcome=%s x%d)",
			issueNum, st.LoopStall.Outcome, st.LoopStall.Count)
	}
	return nil
}

// recordLoopOutcome updates the LoopStall tracker in state. If the
// same issue produces the same outcome as the last run, Count
// increments. Otherwise the tracker resets to (issue, outcome, 1).
func recordLoopOutcome(cwd string, cfg *config.Config, issueNum int, outcome string) {
	st, _ := state.Load(filepath.Join(cwd, cfg.StateDir))
	if st.LoopStall.IssueNumber == issueNum && st.LoopStall.Outcome == outcome {
		st.LoopStall.Count++
	} else {
		st.LoopStall = state.LoopStall{IssueNumber: issueNum, Outcome: outcome, Count: 1}
	}
	_ = state.Save(filepath.Join(cwd, cfg.StateDir), st)
	if st.LoopStall.Count > 1 {
		ui.Note("loop stall counter: issue #%d outcome=%q count=%d/%d",
			issueNum, outcome, st.LoopStall.Count, StallThreshold)
	}
}

// resetLoopStall clears the stall tracker in state.
func resetLoopStall(cwd string, cfg *config.Config) {
	st, _ := state.Load(filepath.Join(cwd, cfg.StateDir))
	if st.LoopStall.Count == 0 && st.LoopStall.IssueNumber == 0 {
		return
	}
	st.LoopStall = state.LoopStall{}
	_ = state.Save(filepath.Join(cwd, cfg.StateDir), st)
}
