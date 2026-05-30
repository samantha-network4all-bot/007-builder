// Package plan asks the LLM to choose the next vertical slice given
// the PRD and the list of already-closed slices.
package plan

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
	"github.com/samantha-network4all-bot/007-builder/internal/sh"
	"github.com/samantha-network4all-bot/007-builder/internal/stream"
	"github.com/samantha-network4all-bot/007-builder/internal/tmpl"
	"github.com/samantha-network4all-bot/007-builder/internal/ui"
)

// LLMResponse is the JSON shape the planner expects from the LLM.
type LLMResponse struct {
	Done   bool     `json:"done"`
	Reason string   `json:"reason,omitempty"`
	Title  string   `json:"title,omitempty"`
	Body   string   `json:"body,omitempty"`
	Labels []string `json:"labels,omitempty"`
}

// NextIssue reads PRD + closed slice list, asks the LLM for the next
// vertical slice, and opens a GitHub issue. Prints the new issue
// number on success.
//
// Flags:
//
//	--dry-run   Render + invoke the LLM, but print the proposed issue
//	            instead of opening it.
//	--config    Path to .agent/config.yaml.
func NextIssue(args []string) error {
	fs := flag.NewFlagSet("next-issue", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "print the proposed issue without opening it")
	configPath := fs.String("config", "", "path to .agent/config.yaml")
	caveman := fs.Bool("caveman", false, "terse mode: prepend a 'no monologues' directive to the prompt")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	cp := *configPath
	if cp == "" {
		cp = cwd
	}
	cfg, err := config.Load(cp)
	if err != nil {
		return err
	}
	if err := cfg.Validate(config.RequireProjectRepo, config.RequirePRDPath, config.RequirePromptsDir); err != nil {
		return err
	}
	ui.Step("planner: asking %s for the next slice", cfg.LLMModel)

	prdPath := absInProject(cwd, cfg.PRDPath)
	prdBytes, err := os.ReadFile(prdPath)
	if err != nil {
		return fmt.Errorf("read PRD %s: %w", prdPath, err)
	}

	closed, err := github.ListSlices(cfg.SliceLabel)
	if err != nil {
		return fmt.Errorf("list closed slices: %w", err)
	}
	closedFiltered := filterClosed(closed)
	closedJSON, _ := json.MarshalIndent(closedFiltered, "", "  ")

	// Build a set of closed issue titles for duplicate detection.
	closedTitles := make([]string, 0, len(closedFiltered))
	closedTitleSet := make(map[string]bool, len(closedFiltered))
	for _, c := range closedFiltered {
		t := strings.ToLower(strings.TrimSpace(c.Title))
		if t != "" {
			closedTitles = append(closedTitles, c.Title)
			closedTitleSet[t] = true
		}
	}

	files, _ := sh.Run(cwd, "git", "ls-files")

	tmplPath := absInProject(cwd, filepath.Join(cfg.PromptsDir, "PROMPT-next-issue.tmpl"))
	tmpPrompt, err := tmpl.RenderToTemp("builder-prompt-", tmplPath, map[string]any{
		"PRD":          string(prdBytes),
		"ClosedJSON":   string(closedJSON),
		"RepoFiles":    files.Stdout,
		"ClosedTitles": closedTitles,
	})
	if err != nil {
		return err
	}
	defer os.Remove(tmpPrompt)

	// Ask the planner, validate, and re-sample on a bad roll. The planner
	// is an LLM and occasionally returns a duplicate slice or malformed
	// acceptance JSON (e.g. mangled query strings like `?x=400&y":300`).
	// Opening such an issue burns the entire attempt budget because the
	// feature check can never parse the probes — exactly how S6 (Fill
	// tool) hit the cap. Re-ask instead of opening a doomed issue.
	const planTries = 3
	var chosen *LLMResponse
	var lastErr error
	var lastRaw string // raw assistant text of the most recent try, for troubleshooting
	for try := 0; try < planTries; try++ {
		// Run in --mode json so we can stream pi's event stream and show
		// live progress; the sink reassembles the final assistant text.
		sink := stream.NewEventSink(false)
		inv := llm.Invocation{
			CLI:              cfg.LLMCLI,
			Model:            cfg.LLMModel,
			Thinking:         cfg.LLMThinking,
			Mode:             "json",
			SystemPromptFile: tmpPrompt,
			UserMessage:      "Choose the next slice. Output strict JSON only — no surrounding prose, no markdown fences.",
			Tools:            "read,grep,find,ls",
			WorkingDir:       cwd,
			Caveman:          *caveman || cfg.LLMCaveman,
			Skills:           cfg.ResolveCodeSkills(cwd),
			Stream:           sink,
		}
		res, err := llm.Run(inv)
		sink.Finish()
		if err != nil {
			lastErr = fmt.Errorf("invoke LLM: %w", err)
			continue
		}
		if res.Outcome != llm.OutcomeSucceeded {
			lastErr = fmt.Errorf("planner outcome=%s exit=%d", res.Outcome, res.ExitCode)
			continue
		}

		// The assistant's final text is in the sink (not res.Stdout).
		lastRaw = sink.AssistantText()
		resp, err := decodeLLMJSON(lastRaw)
		if err != nil {
			lastErr = fmt.Errorf("parse planner JSON: %w", err)
			ui.Warn("planner JSON did not parse (try %d/%d) — re-asking", try+1, planTries)
			continue
		}
		if resp.Done {
			ui.OK("planner: PRD covered — %s", resp.Reason)
			return nil
		}
		if resp.Title == "" || resp.Body == "" {
			lastErr = fmt.Errorf("planner returned no title or body")
			ui.Warn("planner returned no title/body (try %d/%d) — re-asking", try+1, planTries)
			continue
		}

		// Duplicate-title guard (exact + fuzzy): re-ask on a hit.
		proposed := strings.ToLower(strings.TrimSpace(resp.Title))
		if closedTitleSet[proposed] {
			lastErr = fmt.Errorf("duplicate of closed issue %q", resp.Title)
			ui.Warn("planner re-proposed closed issue %q (try %d/%d) — re-asking", resp.Title, try+1, planTries)
			continue
		}
		dup := false
		for closedTitle := range closedTitleSet {
			if strings.Contains(proposed, closedTitle) || strings.Contains(closedTitle, proposed) {
				dup = true
				lastErr = fmt.Errorf("near-duplicate of closed issue %q ≈ %q", resp.Title, closedTitle)
				break
			}
		}
		if dup {
			ui.Warn("planner proposed near-duplicate %q (try %d/%d) — re-asking", resp.Title, try+1, planTries)
			continue
		}

		// Acceptance-block guard: the issue MUST carry a parseable
		// ```json acceptance block, or the feature check can never run
		// and the slice is unwinnable.
		if err := validateAcceptance(resp.Body); err != nil {
			lastErr = err
			ui.Warn("planner acceptance JSON invalid (try %d/%d): %v — re-asking", try+1, planTries, err)
			continue
		}

		chosen = resp
		break
	}
	if chosen == nil {
		// Dump the exact text the planner returned on the final try so the
		// real failure (truncated JSON, prose wrapping, an empty response,
		// a refusal) is visible instead of just the parse error. This is a
		// hard stop: the loop cannot proceed without a valid slice.
		ui.Header("planner: last raw output (%d bytes) — could not produce a valid slice", len(lastRaw))
		if strings.TrimSpace(lastRaw) == "" {
			fmt.Println("<empty — the model returned no assistant text>")
		} else {
			fmt.Println(lastRaw)
		}
		fmt.Println()
		return fmt.Errorf("planner failed to produce a valid slice after %d tries: %w", planTries, lastErr)
	}

	labels := chosen.Labels
	if len(labels) == 0 {
		labels = []string{cfg.SliceLabel, cfg.AttemptLabelPrefix + "0"}
	}

	if *dryRun {
		ui.Header("proposed issue (dry run)")
		ui.KV("title", chosen.Title)
		ui.KV("labels", strings.Join(labels, ","))
		fmt.Println()
		fmt.Println(chosen.Body)
		return nil
	}

	num, err := github.CreateIssue(chosen.Title, chosen.Body, labels)
	if err != nil {
		return fmt.Errorf("create issue: %w", err)
	}
	ui.OK("opened issue")
	ui.Issue(num, chosen.Title)
	return nil
}

func filterClosed(in []github.Issue) []github.Issue {
	out := make([]github.Issue, 0, len(in))
	for _, i := range in {
		if strings.EqualFold(i.State, "CLOSED") {
			out = append(out, i)
		}
	}
	return out
}

func absInProject(cwd, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(cwd, p)
}

// decodeLLMJSON pulls the first decodable LLMResponse out of stdout.
// Handles three shapes the model might emit:
//  1. Bare JSON object.
//  2. Markdown ```json``` fence wrapping the object.
//  3. JSON object preceded/followed by prose (we take the largest {…} span).
func decodeLLMJSON(stdout string) (*LLMResponse, error) {
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return nil, fmt.Errorf("empty stdout")
	}

	// 2) Strip ```json ... ``` if present (anywhere in the text).
	if i := strings.Index(stdout, "```"); i >= 0 {
		rest := stdout[i+3:]
		// Allow either ```json\n or just ```\n.
		rest = strings.TrimPrefix(rest, "json")
		rest = strings.TrimPrefix(rest, "JSON")
		rest = strings.TrimLeft(rest, " \t\r\n")
		if end := strings.Index(rest, "```"); end > 0 {
			candidate := strings.TrimSpace(rest[:end])
			if r, err := tryDecode(candidate); err == nil {
				return r, nil
			}
		}
	}

	// 1) Try direct decode.
	if r, err := tryDecode(stdout); err == nil {
		return r, nil
	}

	// 3) Take the largest {...} span.
	first := strings.Index(stdout, "{")
	last := strings.LastIndex(stdout, "}")
	if first >= 0 && last > first {
		if r, err := tryDecode(stdout[first : last+1]); err == nil {
			return r, nil
		}
	}
	return nil, fmt.Errorf("no decodable JSON in stdout")
}

func tryDecode(s string) (*LLMResponse, error) {
	var r LLMResponse
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return nil, err
	}
	if !r.Done && r.Title == "" {
		return nil, fmt.Errorf("decoded but empty (no done, no title)")
	}
	return &r, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(truncated)"
}

// validateAcceptance confirms the issue body carries a parseable ```json
// acceptance block with at least one well-formed probe. This is the
// guard that would have stopped S6 (Fill tool) from being opened with
// mangled query-string JSON and burning all 10 attempts: a slice whose
// probes don't parse can never pass the feature check, so it must never
// be opened.
func validateAcceptance(body string) error {
	block := extractAcceptanceBlock(body)
	if strings.TrimSpace(block) == "" {
		return fmt.Errorf("issue body has no ```json acceptance block")
	}
	var acc checks.Acceptance
	if err := json.Unmarshal([]byte(block), &acc); err != nil {
		return fmt.Errorf("acceptance JSON does not parse: %w", err)
	}
	if len(acc.Acceptance) == 0 {
		return fmt.Errorf("acceptance block has no steps")
	}
	for i, step := range acc.Acceptance {
		if len(step.Calls) == 0 {
			return fmt.Errorf("acceptance step %d (%q) has no calls", i, step.Step)
		}
		for j, c := range step.Calls {
			if c.Method == "" || !strings.HasPrefix(c.Path, "/") {
				return fmt.Errorf("acceptance step %d call %d: bad method/path (%q %q)", i, j, c.Method, c.Path)
			}
		}
	}
	return nil
}

// extractAcceptanceBlock returns the contents of the first ```json fenced
// block in the issue body (the acceptance probes), or "" if absent.
func extractAcceptanceBlock(body string) string {
	const fence = "```json"
	start := strings.Index(body, fence)
	if start < 0 {
		return ""
	}
	rest := body[start+len(fence):]
	end := strings.Index(rest, "```")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}
