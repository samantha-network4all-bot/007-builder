// Package scaffold lays down the project directory structure that the
// orchestrator needs (.agent/config.yaml, prompt templates, skills,
// Project.yml, .gitignore, lessons-learned.md, README.md) from an
// embedded copy of the slate example.
//
// It is the file-writing half of the `init` command; main.go pairs it
// with loop.Bootstrap to also create the GitHub repo and push the seed
// commit. Scaffold writes only files that do not already exist (unless
// Force is set), so it is safe to re-run.
package scaffold

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/samantha-network4all-bot/007-builder/internal/sh"
	"github.com/samantha-network4all-bot/007-builder/internal/ui"
)

//go:embed all:templates
var templates embed.FS

// Options controls a scaffold run.
type Options struct {
	Dir    string // project root; defaults to cwd
	Name   string // project name; derived from PRD.md title if empty
	Repo   string // owner/name; derived from `gh api user` if empty
	Force  bool   // overwrite existing files instead of skipping them
	DryRun bool   // print actions without writing
}

// Result reports what a scaffold run resolved and did.
type Result struct {
	Name    string
	Repo    string
	Written []string
	Skipped []string
}

// Run scaffolds the project structure into opts.Dir. It requires a
// PRD.md to already exist there — that is the single hand-authored
// input; everything else is generated.
func Run(opts Options) (*Result, error) {
	dir := opts.Dir
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("getwd: %w", err)
		}
		dir = cwd
	}

	prdPath := filepath.Join(dir, "PRD.md")
	prd, err := os.ReadFile(prdPath)
	if err != nil {
		return nil, fmt.Errorf("init requires a PRD.md in %s (write the product spec first): %w", dir, err)
	}

	name := opts.Name
	if name == "" {
		name = deriveName(string(prd), dir)
	}
	if name == "" {
		return nil, fmt.Errorf("could not determine project name from PRD.md title; pass --name")
	}

	repo := opts.Repo
	if repo == "" {
		repo, err = deriveRepo(dir, name)
		if err != nil {
			return nil, err
		}
	}

	rep := strings.NewReplacer(
		"__PROJECT_NAME__", name,
		"__PROJECT_NAME_LOWER__", strings.ToLower(name),
		"__PROJECT_NAME_UPPER__", strings.ToUpper(name),
		"__PROJECT_REPO__", repo,
	)

	ui.Header("init / scaffold")
	ui.KV("project", name)
	ui.KV("repo", repo)
	ui.KV("dir", dir)

	res := &Result{Name: name, Repo: repo}

	err = fs.WalkDir(templates, "templates", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		dest := destPath(strings.TrimPrefix(path, "templates/"))
		full := filepath.Join(dir, dest)

		if _, statErr := os.Stat(full); statErr == nil && !opts.Force {
			res.Skipped = append(res.Skipped, dest)
			ui.Note("skip (exists): %s", dest)
			return nil
		}

		raw, readErr := templates.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		out := rep.Replace(string(raw))

		if opts.DryRun {
			ui.Step("would write: %s (%d bytes)", dest, len(out))
			res.Written = append(res.Written, dest)
			return nil
		}

		if mkErr := os.MkdirAll(filepath.Dir(full), 0o755); mkErr != nil {
			return mkErr
		}
		if wErr := os.WriteFile(full, []byte(out), 0o644); wErr != nil {
			return fmt.Errorf("write %s: %w", dest, wErr)
		}
		res.Written = append(res.Written, dest)
		ui.OK("wrote %s", dest)
		return nil
	})
	if err != nil {
		return res, err
	}

	return res, nil
}

// destPath maps an embedded template path (with the "templates/" prefix
// already stripped) to its destination relative to the project root.
//
//	agent/<x>  → .agent/<x>
//	gitignore  → .gitignore   (embed can't carry a leading-dot dir cleanly)
//	<x>        → <x>
func destPath(rel string) string {
	switch {
	case rel == "gitignore":
		return ".gitignore"
	case strings.HasPrefix(rel, "agent/"):
		return ".agent/" + strings.TrimPrefix(rel, "agent/")
	default:
		return rel
	}
}

// deriveName extracts the project name from the first markdown heading
// of the PRD, expected to look like "# PRD — Pigment (…)". Falls back to
// the project directory's base name.
func deriveName(prd, dir string) string {
	for _, line := range strings.Split(prd, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "#") {
			continue
		}
		title := strings.TrimSpace(strings.TrimLeft(line, "#"))
		// Drop a leading "PRD" and any separator (— - :).
		for _, p := range []string{"PRD —", "PRD -", "PRD:", "PRD"} {
			if strings.HasPrefix(title, p) {
				title = strings.TrimSpace(title[len(p):])
				break
			}
		}
		title = strings.TrimLeft(title, "—-: ")
		// Cut at the first parenthesis or comma.
		if i := strings.IndexAny(title, "(,"); i >= 0 {
			title = strings.TrimSpace(title[:i])
		}
		// Take the first whitespace-delimited token as the name.
		if fields := strings.Fields(title); len(fields) > 0 {
			return fields[0]
		}
		break
	}
	return filepath.Base(dir)
}

// deriveRepo forms "owner/name" using the authenticated gh user as the
// owner. Returns an error (asking for --repo) if gh can't answer.
func deriveRepo(dir, name string) (string, error) {
	r, err := sh.Run(dir, "gh", "api", "user", "-q", ".login")
	if err != nil || r.ExitCode != 0 {
		return "", fmt.Errorf("could not determine GitHub owner via `gh api user`; pass --repo owner/%s", name)
	}
	owner := strings.TrimSpace(r.Stdout)
	if owner == "" {
		return "", fmt.Errorf("empty GitHub owner from `gh api user`; pass --repo owner/%s", name)
	}
	return owner + "/" + name, nil
}
