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
	"text/template"

	"github.com/samantha-network4all-bot/007-builder/internal/config"
	"github.com/samantha-network4all-bot/007-builder/internal/github"
	"github.com/samantha-network4all-bot/007-builder/internal/llm"
	"github.com/samantha-network4all-bot/007-builder/internal/sh"
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
	if err := cfg.Validate("project.repo", "paths.prd", "paths.prompts"); err != nil {
		return err
	}

	prdPath := absInProject(cwd, cfg.PRDPath)
	prdBytes, err := os.ReadFile(prdPath)
	if err != nil {
		return fmt.Errorf("read PRD %s: %w", prdPath, err)
	}

	closed, err := github.ListSlices(cfg.SliceLabel)
	if err != nil {
		return fmt.Errorf("list closed slices: %w", err)
	}
	closedJSON, _ := json.MarshalIndent(filterClosed(closed), "", "  ")

	files, _ := sh.Run(cwd, "git", "ls-files")

	tmplPath := absInProject(cwd, filepath.Join(cfg.PromptsDir, "PROMPT-next-issue.tmpl"))
	rendered, err := renderTemplate(tmplPath, map[string]any{
		"PRD":        string(prdBytes),
		"ClosedJSON": string(closedJSON),
		"RepoFiles":  files.Stdout,
	})
	if err != nil {
		return err
	}

	tmpPrompt, err := writeTempPrompt(rendered)
	if err != nil {
		return err
	}
	defer os.Remove(tmpPrompt)

	inv := llm.Invocation{
		CLI:              cfg.LLMCLI,
		Mode:             "json",
		SystemPromptFile: tmpPrompt,
		UserMessage:      "Choose the next slice. Output strict JSON only.",
		Tools:            "read,grep,find,ls",
		WorkingDir:       cwd,
	}
	res, err := llm.Run(inv)
	if err != nil {
		return fmt.Errorf("invoke LLM: %w", err)
	}
	if res.Outcome != llm.OutcomeSucceeded {
		return fmt.Errorf("planner outcome=%s exit=%d\nstderr:\n%s", res.Outcome, res.ExitCode, truncate(res.Stderr, 2000))
	}

	resp, err := decodeLLMJSON(res.Stdout)
	if err != nil {
		return fmt.Errorf("parse planner JSON: %w\nstdout was:\n%s", err, truncate(res.Stdout, 2000))
	}

	if resp.Done {
		fmt.Printf("planner: done — %s\n", resp.Reason)
		return nil
	}
	if resp.Title == "" || resp.Body == "" {
		return fmt.Errorf("planner returned no title or body: %#v", resp)
	}

	labels := resp.Labels
	if len(labels) == 0 {
		labels = []string{cfg.SliceLabel, cfg.AttemptLabelPrefix + "0"}
	}

	if *dryRun {
		fmt.Println("=== proposed issue ===")
		fmt.Println("title:", resp.Title)
		fmt.Println("labels:", strings.Join(labels, ","))
		fmt.Println("body:")
		fmt.Println(resp.Body)
		return nil
	}

	num, err := github.CreateIssue(resp.Title, resp.Body, labels)
	if err != nil {
		return fmt.Errorf("create issue: %w", err)
	}
	fmt.Printf("opened issue #%d: %s\n", num, resp.Title)
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

// decodeLLMJSON pulls the first decodable LLMResponse out of stdout.
// Accepts a raw object or a `{"result": "..."}` envelope (pi --mode json
// can produce either depending on the model wrapper).
func decodeLLMJSON(stdout string) (*LLMResponse, error) {
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return nil, fmt.Errorf("empty stdout")
	}

	var direct LLMResponse
	if err := json.Unmarshal([]byte(stdout), &direct); err == nil && (direct.Done || direct.Title != "") {
		return &direct, nil
	}

	first := strings.Index(stdout, "{")
	last := strings.LastIndex(stdout, "}")
	if first >= 0 && last > first {
		blob := stdout[first : last+1]
		// envelope { "result": ... } where result is the payload (object or stringified JSON).
		var envelope struct {
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal([]byte(blob), &envelope); err == nil && len(envelope.Result) > 0 {
			var inner LLMResponse
			if err := json.Unmarshal(envelope.Result, &inner); err == nil && (inner.Done || inner.Title != "") {
				return &inner, nil
			}
			var unquoted string
			if err := json.Unmarshal(envelope.Result, &unquoted); err == nil {
				if err := json.Unmarshal([]byte(unquoted), &inner); err == nil && (inner.Done || inner.Title != "") {
					return &inner, nil
				}
			}
		}
		if err := json.Unmarshal([]byte(blob), &direct); err == nil && (direct.Done || direct.Title != "") {
			return &direct, nil
		}
	}
	return nil, fmt.Errorf("no decodable JSON in stdout")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(truncated)"
}
