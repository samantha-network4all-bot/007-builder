// Package review runs the thermo-nuclear code-quality sweep against
// the cumulative diff of the last N green slices, and opens
// refactor:thermonuclear issues for every blocker the LLM flags.
//
// The loop fires this after every 5 *consecutive* successful slices
// (counter resets on any failure). The orchestrator picks up
// refactor:thermonuclear issues before any feature slice, so
// structural debt is paid down before it compounds.
package review

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/samantha-network4all-bot/007-builder/internal/config"
	"github.com/samantha-network4all-bot/007-builder/internal/github"
	"github.com/samantha-network4all-bot/007-builder/internal/llm"
	"github.com/samantha-network4all-bot/007-builder/internal/sh"
	"github.com/samantha-network4all-bot/007-builder/internal/stream"
	"github.com/samantha-network4all-bot/007-builder/internal/tmpl"
	"github.com/samantha-network4all-bot/007-builder/internal/ui"
)

const (
	// RefactorLabel marks issues opened by this package. The loop's
	// issue picker prefers these over `slice`-labelled issues so the
	// codebase gets re-paved before more features land on it.
	RefactorLabel = "refactor:thermonuclear"

	// PromptFile is the rendered template inside the project's prompt
	// directory.
	PromptFile = "PROMPT-thermo-nuclear.tmpl"
)

// LLMResponse is the JSON shape we expect from the review skill.
type LLMResponse struct {
	Verdict  string    `json:"verdict"` // "approve" | "request-changes"
	Blockers []Blocker `json:"blockers"`
	Nits     []Nit     `json:"nits"`
}

// Blocker is one structural finding worth its own issue.
type Blocker struct {
	Title     string   `json:"title"`
	Files     []string `json:"files"`
	Category  string   `json:"category"`
	Rationale string   `json:"rationale"`
}

// Nit is a non-blocking note. Not turned into issues.
type Nit struct {
	File      string `json:"file"`
	Rationale string `json:"rationale"`
}

// Run renders the thermo prompt, invokes pi with the review skills,
// parses the JSON verdict, and opens one issue per blocker. baseSHA
// is the commit immediately *before* the window — diff = baseSHA..HEAD.
//
// Returns the count of refactor issues opened (0 means clean review).
func Run(cwd string, cfg *config.Config, baseSHA string, slicesInWindow []github.Issue) (int, error) {
	ui.Header("thermo-nuclear review")
	ui.KV("window", fmt.Sprintf("last %d slices", len(slicesInWindow)))
	ui.KV("baseSHA", abbrev(baseSHA))

	diff, err := capturedDiff(cwd, baseSHA)
	if err != nil {
		return 0, fmt.Errorf("git diff: %w", err)
	}

	tmplPath := filepath.Join(cwd, cfg.PromptsDir, PromptFile)
	tmpPrompt, err := tmpl.RenderToTemp("builder-thermo-", tmplPath, map[string]any{
		"WindowSize":  len(slicesInWindow),
		"BaseSHA":     baseSHA,
		"DiffSummary": diff,
		"Slices":      slicesInWindow,
	})
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmpPrompt)

	sink := stream.NewEventSink(false)
	inv := llm.Invocation{
		CLI:              cfg.LLMCLI,
		Model:            cfg.LLMModel,
		Thinking:         cfg.LLMThinking,
		Mode:             "json",
		SystemPromptFile: tmpPrompt,
		UserMessage:      "Run the thermo-nuclear review on the diff above. Output strict JSON only, per the skill's output-format section.",
		Tools:            "read,grep,find,ls",
		WorkingDir:       cwd,
		Caveman:          cfg.LLMCaveman, // always tightest mode for reviews
		Skills:           cfg.ResolveReviewSkills(cwd),
		Stream:           sink,
	}
	res, err := llm.Run(inv)
	sink.Finish()
	if err != nil {
		return 0, fmt.Errorf("invoke reviewer: %w", err)
	}
	if res.Outcome != llm.OutcomeSucceeded {
		return 0, fmt.Errorf("reviewer outcome=%s exit=%d\nstderr:\n%s",
			res.Outcome, res.ExitCode, truncate(res.Stderr, 2000))
	}

	resp, err := parseJSON(sink.AssistantText())
	if err != nil {
		return 0, fmt.Errorf("parse review JSON: %w\nassistant text was:\n%s",
			err, truncate(sink.AssistantText(), 2000))
	}

	if resp.Verdict == "approve" || len(resp.Blockers) == 0 {
		ui.OK("thermo-nuclear: clean (%d nits, no blockers)", len(resp.Nits))
		return 0, nil
	}

	for i, b := range resp.Blockers {
		title := b.Title
		if !strings.HasPrefix(strings.ToLower(title), "refactor:") {
			title = "refactor: " + title
		}
		body := buildIssueBody(b, slicesInWindow)
		num, err := github.CreateIssue(title, body, []string{RefactorLabel})
		if err != nil {
			ui.Fail("blocker %d: %s — %v", i+1, b.Title, err)
			continue
		}
		ui.OK("opened refactor issue #%d: %s", num, title)
	}
	return len(resp.Blockers), nil
}

func buildIssueBody(b Blocker, slices []github.Issue) string {
	var sb strings.Builder
	sb.WriteString("## Category\n\n")
	sb.WriteString("`" + b.Category + "`\n\n")
	sb.WriteString("## Rationale\n\n")
	sb.WriteString(b.Rationale + "\n\n")
	if len(b.Files) > 0 {
		sb.WriteString("## Files\n\n")
		for _, f := range b.Files {
			sb.WriteString("- `" + f + "`\n")
		}
		sb.WriteString("\n")
	}
	if len(slices) > 0 {
		sb.WriteString("## Review window\n\nFlagged after the cumulative diff of:\n\n")
		for _, s := range slices {
			sb.WriteString(fmt.Sprintf("- #%d %s\n", s.Number, s.Title))
		}
		sb.WriteString("\n")
	}
	sb.WriteString("## Acceptance\n\n")
	sb.WriteString("Opened by the thermo-nuclear review. Treat as a structural refactor — no new features, no scope creep. ")
	sb.WriteString("Definition of done is the absence of the structural problem named above.\n\n")
	sb.WriteString("```json\n{\"acceptance\":[]}\n```\n\n")
	sb.WriteString("_No HTTP probes — this is a structural change; the next thermo-nuclear sweep will re-evaluate._\n")
	return sb.String()
}

func capturedDiff(cwd, baseSHA string) (string, error) {
	if baseSHA == "" {
		baseSHA = "HEAD~1"
	}
	r, err := sh.MustRun(cwd, "git", "diff", "--stat", baseSHA+"..HEAD")
	if err != nil {
		return "", err
	}
	full, err := sh.MustRun(cwd, "git", "diff", baseSHA+"..HEAD")
	if err != nil {
		return "", err
	}
	// Stat first for the model to see what changed at a glance, then
	// the full diff. Truncate the full diff at 200KB so we don't blow
	// the context.
	body := r.Stdout + "\n\n" + truncate(full.Stdout, 200_000)
	return body, nil
}

// parseJSON extracts the LLMResponse from the assistant's text. Same
// laxer rules as plan.decodeLLMJSON: bare JSON, fenced JSON, or
// largest {…} span.
func parseJSON(text string) (*LLMResponse, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("empty assistant text")
	}
	// Try fenced ```json``` first.
	if i := strings.Index(text, "```"); i >= 0 {
		rest := text[i+3:]
		rest = strings.TrimPrefix(rest, "json")
		rest = strings.TrimPrefix(rest, "JSON")
		rest = strings.TrimLeft(rest, " \t\r\n")
		if end := strings.Index(rest, "```"); end > 0 {
			if r, err := tryDecode(strings.TrimSpace(rest[:end])); err == nil {
				return r, nil
			}
		}
	}
	if r, err := tryDecode(text); err == nil {
		return r, nil
	}
	first := strings.Index(text, "{")
	last := strings.LastIndex(text, "}")
	if first >= 0 && last > first {
		if r, err := tryDecode(text[first : last+1]); err == nil {
			return r, nil
		}
	}
	return nil, fmt.Errorf("no decodable JSON in assistant text")
}

func tryDecode(s string) (*LLMResponse, error) {
	var r LLMResponse
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return nil, err
	}
	if r.Verdict == "" {
		return nil, fmt.Errorf("decoded but missing verdict")
	}
	return &r, nil
}

func abbrev(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(truncated)"
}
