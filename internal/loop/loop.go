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
	"text/template"

	"github.com/samantha-network4all-bot/007-builder/internal/config"
	"github.com/samantha-network4all-bot/007-builder/internal/github"
	"github.com/samantha-network4all-bot/007-builder/internal/llm"
	"github.com/samantha-network4all-bot/007-builder/internal/plan"
	"github.com/samantha-network4all-bot/007-builder/internal/sh"
	"github.com/samantha-network4all-bot/007-builder/internal/state"
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
	if err := cfg.Validate("project.repo", "project.name"); err != nil {
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
	if err := cfg.Validate("project.repo", "paths.prd", "paths.prompts", "feature_test.binary"); err != nil {
		return err
	}

	// Refuse to start on a dirty tree.
	if r, _ := sh.Run(cwd, "git", "status", "--porcelain"); strings.TrimSpace(r.Stdout) != "" {
		return fmt.Errorf("working tree is dirty; refusing to start work:\n%s", r.Stdout)
	}

	// Pick issue.
	var issue *github.Issue
	if *issueOverride > 0 {
		issue, err = github.GetIssue(*issueOverride)
		if err != nil {
			return err
		}
	} else {
		issue, err = github.OldestOpenSlice(cfg.SliceLabel)
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

	// Render the code prompt.
	tmplPath := filepath.Join(cwd, cfg.PromptsDir, "PROMPT-code.tmpl")
	acceptance := extractAcceptanceBlock(issue.Body)
	rendered, err := renderTemplate(tmplPath, map[string]any{
		"IssueNumber":     issue.Number,
		"IssueTitle":      issue.Title,
		"IssueBody":       issue.Body,
		"AcceptanceJSON":  acceptance,
		"PreviousFailure": readPreviousFailure(cwd, cfg),
	})
	if err != nil {
		return err
	}

	tmpPrompt, err := writeTempPrompt(rendered)
	if err != nil {
		return err
	}
	defer os.Remove(tmpPrompt)

	if *dryRun {
		ui.Header("rendered code prompt")
		fmt.Println(rendered)
		return nil
	}

	ui.Step("invoking code agent (%s %s)", cfg.LLMCLI, cfg.LLMModel)
	// Invoke the code agent.
	// Streaming sink renders pi's live events (tool calls + char counter)
	// so the user sees progress instead of waiting in silence for what
	// can be 5-10 minutes of agent work.
	sink := llm.NewEventSink(false)
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
		// Bump attempt and record context.
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
			rawURL := github.RepoRawURL(cfg.ProjectRepo, "main", shotPath)
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
		newAttempt := attempt + 1
		_ = github.SetAttemptLabel(issue.Number, cfg.AttemptLabelPrefix, newAttempt)
		if newAttempt >= cfg.AttemptsPerIssue {
			return github.HandoffForReview(issue.Number, cfg.HITLLabel,
				fmt.Sprintf("feature check failed after %d attempts", newAttempt))
		}
		return fmt.Errorf("feature check failed; bumped to attempt %d", newAttempt)
	}

	// Green. Close the issue.
	if err := github.CloseIssue(issue.Number,
		fmt.Sprintf("Closed by 007-builder. Commit %s passed the feature check.", abbrev(res.HEADAfter))); err != nil {
		return err
	}
	ui.OK("closed issue #%d", issue.Number)
	return nil
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
		open, err := github.OldestOpenSlice(cfg.SliceLabel)
		if err != nil {
			return fmt.Errorf("iter %d: list open slices: %w", i, err)
		}
		if open == nil {
			ui.Note("no open slice — asking planner for a new one")
			if err := plan.NextIssue(inner); err != nil {
				return fmt.Errorf("iter %d: next-issue: %w", i, err)
			}
		} else {
			ui.Note("continuing on open issue #%d", open.Number)
		}

		if err := Work(inner); err != nil {
			ui.Fail("iter %d: work returned: %v", i, err)
			// continue — failed iterations bump attempt; loop will pick it back up.
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

func renderTemplate(path string, data any) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read template %s: %w", path, err)
	}
	t, err := template.New(filepath.Base(path)).Parse(string(b))
	if err != nil {
		return "", fmt.Errorf("parse template %s: %w", path, err)
	}
	var sb strings.Builder
	if err := t.Execute(&sb, data); err != nil {
		return "", fmt.Errorf("execute template %s: %w", path, err)
	}
	return sb.String(), nil
}

func writeTempPrompt(s string) (string, error) {
	f, err := os.CreateTemp("", "builder-prompt-*.md")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
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

func runFeatureCheck(cwd string, cfg *config.Config, acceptanceJSON string, issueNum, attempt int) error {
	// Write the acceptance JSON to a temp file and invoke checks.Feature
	// via the builder binary itself — easier than re-importing checks here.
	tmp, err := os.CreateTemp("", "probes-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(acceptanceJSON); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	r, err := sh.Run(cwd, exe, "check", "feature",
		"--probes", tmp.Name(),
		"--issue", fmt.Sprintf("%d", issueNum),
		"--attempt", fmt.Sprintf("%d", attempt),
	)
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return fmt.Errorf("check feature exit=%d\n%s", r.ExitCode, r.Combined())
	}
	fmt.Print(r.Stdout)
	return nil
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
