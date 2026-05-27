// Package loop owns the high-level orchestrator commands: bootstrap,
// work, and run (the full iterative loop). The per-issue state machine
// also lives here.
package loop

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/samantha-network4all-bot/007-builder/internal/config"
	"github.com/samantha-network4all-bot/007-builder/internal/github"
	"github.com/samantha-network4all-bot/007-builder/internal/sh"
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

	fmt.Printf("bootstrap: project=%s repo=%s cwd=%s\n", cfg.ProjectName, cfg.ProjectRepo, cwd)

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
		fmt.Printf("created repo: https://github.com/%s\n", cfg.ProjectRepo)
	} else {
		fmt.Printf("repo already exists: https://github.com/%s\n", cfg.ProjectRepo)
	}

	if err := ensureLocalGit(cwd, cfg.ProjectRepo); err != nil {
		return err
	}

	if err := initialCommitAndPush(cwd, cfg.ProjectName); err != nil {
		return err
	}

	fmt.Println("bootstrap complete.")
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
		fmt.Println("created seed commit.")
	} else {
		fmt.Println("working tree clean — nothing new to commit.")
	}

	push, err := sh.Run(dir, "git", "push", "-u", "origin", "main")
	if err != nil {
		return err
	}
	if push.ExitCode != 0 {
		return fmt.Errorf("git push failed:\n%s", push.Combined())
	}
	fmt.Println("pushed to origin/main.")
	return nil
}

// Work picks the oldest open `slice` issue, invokes the code agent,
// runs the two checks, and either closes the issue (green) or bumps
// attempt:N (red, with N<cap) or hands off to a human (N==cap).
//
// TODO(007-builder): implement after the LLM invocation layer
// stabilises in internal/llm.
func Work(args []string) error {
	return fmt.Errorf("TODO: work not yet implemented")
}

// Run repeats next-issue + work until next-issue says "PRD complete".
//
// TODO(007-builder): implement after Work lands.
func Run(args []string) error {
	return fmt.Errorf("TODO: loop not yet implemented")
}
