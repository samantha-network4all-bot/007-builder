package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleYAML = `# Sample config for the test
project:
  name: slate
  repo: samantha-network4all-bot/slate-v2

paths:
  prd: PRD.md
  lessons: lessons-learned.md
  prompts: .agent/prompts
  state: .slate

llm:
  cli: pi
  mode: json
  tools: read,bash,edit,write,grep,find,ls

loop:
  attempts_per_issue: 10
  hitl_label: awaiting-human-review
  slice_label: slice
  attempt_label_prefix: "attempt:"

feature_test:
  enable_env: SLATE_TEST_API=1
  port_file: ~/Library/Application Support/Slate/test-api.port
  build:
    - xcodegen generate
    - xcodebuild -scheme Slate -configuration Debug -derivedDataPath build/ build
  binary: build/Build/Products/Debug/Slate.app/Contents/MacOS/Slate
  shutdown_endpoint: /shutdown
  healthz_endpoint: /healthz
  healthz_timeout_seconds: 10
`

func TestLoadFlat(t *testing.T) {
	dir := t.TempDir()
	agentDir := filepath.Join(dir, ".agent")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "config.yaml"), []byte(sampleYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	checks := []struct {
		got, want, name string
	}{
		{cfg.ProjectName, "slate", "ProjectName"},
		{cfg.ProjectRepo, "samantha-network4all-bot/slate-v2", "ProjectRepo"},
		{cfg.PRDPath, "PRD.md", "PRDPath"},
		{cfg.PromptsDir, ".agent/prompts", "PromptsDir"},
		{cfg.LLMCLI, "pi", "LLMCLI"},
		{cfg.LLMMode, "json", "LLMMode"},
		{cfg.HITLLabel, "awaiting-human-review", "HITLLabel"},
		{cfg.SliceLabel, "slice", "SliceLabel"},
		{cfg.AttemptLabelPrefix, "attempt:", "AttemptLabelPrefix"},
		{cfg.FeatureEnableEnv, "SLATE_TEST_API=1", "FeatureEnableEnv"},
		{cfg.FeatureBinary, "build/Build/Products/Debug/Slate.app/Contents/MacOS/Slate", "FeatureBinary"},
		{cfg.FeatureHealthzEndpoint, "/healthz", "FeatureHealthzEndpoint"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %q want %q", c.name, c.got, c.want)
		}
	}

	if cfg.AttemptsPerIssue != 10 {
		t.Errorf("AttemptsPerIssue: got %d want 10", cfg.AttemptsPerIssue)
	}
	if cfg.FeatureHealthzTimeoutS != 10 {
		t.Errorf("FeatureHealthzTimeoutS: got %d want 10", cfg.FeatureHealthzTimeoutS)
	}
	if len(cfg.FeatureBuild) != 2 || cfg.FeatureBuild[0] != "xcodegen generate" {
		t.Errorf("FeatureBuild: %#v", cfg.FeatureBuild)
	}
	if !strings.Contains(cfg.FeaturePortFile, "Library/Application Support/Slate/test-api.port") {
		t.Errorf("FeaturePortFile: %q (expected expanded ~)", cfg.FeaturePortFile)
	}
	if strings.HasPrefix(cfg.FeaturePortFile, "~") {
		t.Errorf("FeaturePortFile not expanded: %q", cfg.FeaturePortFile)
	}
}

func TestValidate(t *testing.T) {
	c := &Config{Path: "x.yaml", ProjectName: "n"}
	err := c.Validate(RequireProjectRepo)
	if err == nil || !strings.Contains(err.Error(), "project.repo") {
		t.Errorf("expected missing-field error, got %v", err)
	}
	c.ProjectRepo = "a/b"
	if err := c.Validate(RequireProjectRepo, RequireProjectName); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
