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

	// Run in --mode json so we can stream pi's event stream and show
	// live progress (tool calls, deltas, char counter). The sink also
	// reassembles the final assistant text from text_delta events.
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
		return fmt.Errorf("invoke LLM: %w", err)
	}
	if res.Outcome != llm.OutcomeSucceeded {
		return fmt.Errorf("planner outcome=%s exit=%d\nstderr:\n%s", res.Outcome, res.ExitCode, truncate(res.Stderr, 2000))
	}

	// The assistant's final text is in the sink (not in res.Stdout —
	// that contains the raw event stream).
	assistant := sink.AssistantText()
	resp, err := decodeLLMJSON(assistant)
	if err != nil {
		return fmt.Errorf("parse planner JSON: %w\nassistant text was:\n%s", err, truncate(assistant, 2000))
	}

	if resp.Done {
		ui.OK("planner: PRD covered — %s", resp.Reason)
		return nil
	}
	if resp.Title == "" || resp.Body == "" {
		return fmt.Errorf("planner returned no title or body: %#v", resp)
	}

	// Duplicate-title guard: if the planner re-proposed a slice
	// whose title matches a closed issue, reject it and ask again.
	proposed := strings.ToLower(strings.TrimSpace(resp.Title))
	if closedTitleSet[proposed] {
		ui.Warn("planner re-proposed closed issue %q — rejecting", resp.Title)
		return fmt.Errorf("planner returned duplicate of closed issue: %q", resp.Title)
	}
	// Fuzzy match: check if the proposed title is a substring of (or
	// contains) any closed title — catches "S1: App scaffolding" vs
	// "S1: App scaffolding — empty borderedless window".
	for closedTitle := range closedTitleSet {
		if strings.Contains(proposed, closedTitle) || strings.Contains(closedTitle, proposed) {
			ui.Warn("planner proposed near-duplicate of closed issue %q → rejecting", resp.Title)
			return fmt.Errorf("planner returned near-duplicate of closed issue: %q ≈ %q", resp.Title, closedTitle)
		}
	}

	labels := resp.Labels
	if len(labels) == 0 {
		labels = []string{cfg.SliceLabel, cfg.AttemptLabelPrefix + "0"}
	}

	if *dryRun {
		ui.Header("proposed issue (dry run)")
		ui.KV("title", resp.Title)
		ui.KV("labels", strings.Join(labels, ","))
		fmt.Println()
		fmt.Println(resp.Body)
		return nil
	}

	num, err := github.CreateIssue(resp.Title, resp.Body, labels)
	if err != nil {
		return fmt.Errorf("create issue: %w", err)
	}
	ui.OK("opened issue")
	ui.Issue(num, resp.Title)
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
