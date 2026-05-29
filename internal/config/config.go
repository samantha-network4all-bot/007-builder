// Package config reads .agent/config.yaml from a project directory.
//
// The YAML reader is intentionally minimal — it handles the flat
// "key: value" and one-level-nested forms used by builder configs,
// without pulling in a third-party YAML library. If the schema
// grows complex we can swap to gopkg.in/yaml.v3 without breaking
// callers.
package config

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Config is the runtime view of .agent/config.yaml.
//
// Only the fields builder actually reads today are populated. Unknown
// keys are silently ignored.
type Config struct {
	// Path the config was loaded from (absolute).
	Path string

	// Project metadata.
	ProjectName string // project.name
	ProjectRepo string // project.repo, e.g. "owner/name"

	// Paths (relative to the project root unless absolute).
	PRDPath      string // paths.prd
	LessonsPath  string // paths.lessons
	PromptsDir   string // paths.prompts
	StateDir     string // paths.state

	// LLM CLI choice.
	LLMCLI      string // llm.cli  ("pi" | "claude")
	LLMMode     string // llm.mode ("json" | "text")
	LLMTools    string // llm.tools
	LLMModel    string // llm.model ("openrouter/owl-alpha", etc.)
	LLMThinking string // llm.thinking ("off"|"minimal"|"low"|"medium"|"high"|"xhigh")
	LLMCaveman  bool   // llm.caveman — terse-mode directive prepended to all prompts

	// Skill files loaded via pi --skill <path>. CodeSkills are loaded on
	// every code-writing turn; ReviewSkills on every code-quality /
	// thermo-nuclear turn. Paths are relative to the project root.
	CodeSkills   []string // skills.code:    [...]
	ReviewSkills []string // skills.review:  [...]

	// Loop.
	AttemptsPerIssue   int    // loop.attempts_per_issue
	HITLLabel          string // loop.hitl_label
	SliceLabel         string // loop.slice_label
	AttemptLabelPrefix string // loop.attempt_label_prefix

	// Feature test.
	FeatureEnableEnv        string   // feature_test.enable_env
	FeaturePortFile         string   // feature_test.port_file
	FeatureBuild            []string // feature_test.build
	FeatureBinary           string   // feature_test.binary
	FeatureShutdownEndpoint string   // feature_test.shutdown_endpoint
	FeatureHealthzEndpoint  string   // feature_test.healthz_endpoint
	FeatureHealthzTimeoutS  int      // feature_test.healthz_timeout_seconds
}

// Load reads .agent/config.yaml from the given directory (or the path
// itself if it looks like a file). Returns a populated Config.
func Load(pathOrDir string) (*Config, error) {
	path := pathOrDir
	info, err := os.Stat(path)
	if err == nil && info.IsDir() {
		path = filepath.Join(path, ".agent", "config.yaml")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve config path: %w", err)
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, fmt.Errorf("open config %s: %w", abs, err)
	}
	defer f.Close()

	c, err := parseFlat(f)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", abs, err)
	}
	c.Path = abs
	return c, nil
}

// parseFlat reads keys of the form "section.subkey: value" or YAML-ish
// nested sections of depth 1. List values ("build:" followed by "- ...")
// are read into a []string.
func parseFlat(r io.Reader) (*Config, error) {
	c := &Config{}
	sc := bufio.NewScanner(r)

	var section string  // current top-level section name, e.g. "project"
	var listKey string  // when non-empty, we're accumulating a "-" list under section.listKey

	for sc.Scan() {
		raw := sc.Text()
		// Strip trailing CR (Windows line endings).
		raw = strings.TrimRight(raw, "\r")
		line := raw
		// Drop comments — only if '#' begins a token after whitespace
		// (avoid trimming '#' inside quoted strings; we don't support quotes here).
		if idx := strings.Index(line, "#"); idx >= 0 {
			line = line[:idx]
		}
		// Trim trailing whitespace; preserve leading for indent detection.
		line = strings.TrimRight(line, " \t")
		if strings.TrimSpace(line) == "" {
			listKey = ""
			continue
		}

		// List item under section.listKey ?
		if listKey != "" && strings.HasPrefix(strings.TrimLeft(line, " \t"), "- ") {
			val := strings.TrimSpace(strings.TrimLeft(line, " \t")[2:])
			val = strings.Trim(val, `"'`)
			setList(c, section, listKey, val)
			continue
		}
		listKey = ""

		// Indented "subkey: value" under current section.
		if strings.HasPrefix(line, "  ") {
			rest := strings.TrimLeft(line, " ")
			k, v, ok := splitKV(rest)
			if !ok {
				continue
			}
			if v == "" {
				// "key:" with no value — could be the start of a list.
				listKey = k
				continue
			}
			set(c, section, k, v)
			continue
		}

		// Top-level "section:" header.
		if strings.HasSuffix(strings.TrimSpace(line), ":") {
			section = strings.TrimSuffix(strings.TrimSpace(line), ":")
			continue
		}

		// Top-level "key: value" (no section).
		if k, v, ok := splitKV(strings.TrimSpace(line)); ok {
			set(c, "", k, v)
		}
	}
	return c, sc.Err()
}

func splitKV(s string) (k, v string, ok bool) {
	i := strings.Index(s, ":")
	if i < 0 {
		return "", "", false
	}
	k = strings.TrimSpace(s[:i])
	v = strings.TrimSpace(s[i+1:])
	v = strings.Trim(v, `"'`)
	return k, v, k != ""
}

// set assigns a parsed value into the right Config field. Unknown
// keys are silently ignored so configs can carry forward-compatible
// extras.
func set(c *Config, section, key, value string) {
	switch section + "." + key {
	case "project.name":
		c.ProjectName = value
	case "project.repo":
		c.ProjectRepo = value
	case "paths.prd":
		c.PRDPath = value
	case "paths.lessons":
		c.LessonsPath = value
	case "paths.prompts":
		c.PromptsDir = value
	case "paths.state":
		c.StateDir = value
	case "llm.cli":
		c.LLMCLI = value
	case "llm.mode":
		c.LLMMode = value
	case "llm.tools":
		c.LLMTools = value
	case "llm.model":
		c.LLMModel = value
	case "llm.thinking":
		c.LLMThinking = value
	case "llm.caveman":
		c.LLMCaveman = value == "true" || value == "1" || value == "yes"
	case "loop.attempts_per_issue":
		fmt.Sscanf(value, "%d", &c.AttemptsPerIssue)
	case "loop.hitl_label":
		c.HITLLabel = value
	case "loop.slice_label":
		c.SliceLabel = value
	case "loop.attempt_label_prefix":
		c.AttemptLabelPrefix = value
	case "feature_test.enable_env":
		c.FeatureEnableEnv = value
	case "feature_test.port_file":
		c.FeaturePortFile = expandHome(value)
	case "feature_test.binary":
		c.FeatureBinary = value
	case "feature_test.shutdown_endpoint":
		c.FeatureShutdownEndpoint = value
	case "feature_test.healthz_endpoint":
		c.FeatureHealthzEndpoint = value
	case "feature_test.healthz_timeout_seconds":
		fmt.Sscanf(value, "%d", &c.FeatureHealthzTimeoutS)
	}
}

func setList(c *Config, section, key, value string) {
	switch section + "." + key {
	case "feature_test.build":
		c.FeatureBuild = append(c.FeatureBuild, value)
	case "skills.code":
		c.CodeSkills = append(c.CodeSkills, value)
	case "skills.review":
		c.ReviewSkills = append(c.ReviewSkills, value)
	}
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// ResolveCodeSkills returns absolute paths for CodeSkills, relative to
// the given project root, dropping any entries that don't exist on
// disk (pi --skill rejects nonexistent paths).
func (c *Config) ResolveCodeSkills(projectRoot string) []string {
	return resolvePaths(projectRoot, c.CodeSkills)
}

// ResolveReviewSkills is the same for ReviewSkills.
func (c *Config) ResolveReviewSkills(projectRoot string) []string {
	return resolvePaths(projectRoot, c.ReviewSkills)
}

func resolvePaths(root string, raw []string) []string {
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		full := p
		if !filepath.IsAbs(full) {
			full = filepath.Join(root, p)
		}
		if _, err := os.Stat(full); err == nil {
			out = append(out, full)
		}
	}
	return out
}

// Required names a config field that a caller insists must be set.
// Using a typed enum (instead of magic strings) catches misspellings
// at compile time — flagged by the thermo-nuclear sweep.
type Required int

const (
	RequireProjectRepo Required = iota
	RequireProjectName
	RequirePRDPath
	RequirePromptsDir
	RequireFeatureBinary
)

func (r Required) String() string {
	switch r {
	case RequireProjectRepo:
		return "project.repo"
	case RequireProjectName:
		return "project.name"
	case RequirePRDPath:
		return "paths.prd"
	case RequirePromptsDir:
		return "paths.prompts"
	case RequireFeatureBinary:
		return "feature_test.binary"
	default:
		return fmt.Sprintf("Required(%d)", int(r))
	}
}

// Validate returns a non-nil error if any required field is missing.
// Bootstrap requires fewer fields than Loop — the caller specifies
// which subset matters via the typed Required enum.
func (c *Config) Validate(required ...Required) error {
	missing := []string{}
	for _, r := range required {
		var ok bool
		switch r {
		case RequireProjectRepo:
			ok = c.ProjectRepo != ""
		case RequireProjectName:
			ok = c.ProjectName != ""
		case RequirePRDPath:
			ok = c.PRDPath != ""
		case RequirePromptsDir:
			ok = c.PromptsDir != ""
		case RequireFeatureBinary:
			ok = c.FeatureBinary != ""
		}
		if !ok {
			missing = append(missing, r.String())
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("config missing required fields: %s (in %s)", strings.Join(missing, ", "), c.Path)
	}
	return nil
}
