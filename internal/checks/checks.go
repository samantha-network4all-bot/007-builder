// Package checks owns the two post-commit gates: code-quality (LLM
// review) and feature-test (build + HTTP probes against the running app).
package checks

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/samantha-network4all-bot/007-builder/internal/config"
	"github.com/samantha-network4all-bot/007-builder/internal/sh"
	"github.com/samantha-network4all-bot/007-builder/internal/ui"
)

// Dispatch routes `builder check <sub> ...` to the right handler.
func Dispatch(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: check <quality|feature> [args]")
	}
	switch args[0] {
	case "quality":
		return Quality(args[1:])
	case "feature":
		return Feature(args[1:])
	default:
		return fmt.Errorf("unknown check subcommand: %q", args[0])
	}
}

// Probe is one HTTP assertion from an issue's acceptance block.
type Probe struct {
	Step  string `json:"step,omitempty"`
	Calls []Call `json:"calls,omitempty"`
}

// Call is one HTTP request + optional response assertion.
type Call struct {
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Body   json.RawMessage `json:"body,omitempty"`
	Expect json.RawMessage `json:"expect,omitempty"`
}

// Acceptance is the top-level JSON in an issue body's ```json``` block.
type Acceptance struct {
	Acceptance []Probe `json:"acceptance"`
}

// FeatureReport is what Feature writes to .slate/checks/feature.json.
type FeatureReport struct {
	StartedAt      string    `json:"startedAt"`
	FinishedAt     string    `json:"finishedAt"`
	Pass           bool      `json:"pass"`
	Steps          []StepRes `json:"steps"`
	BuildLog       string    `json:"buildLog,omitempty"`
	LaunchError    string    `json:"launchError,omitempty"`
	ScreenshotPath string    `json:"screenshotPath,omitempty"` // repo-relative path, e.g. "screenshots/S1-attempt-0.png"
}

// StepRes is the per-step outcome.
type StepRes struct {
	Step       string `json:"step"`
	OK         bool   `json:"ok"`
	Status     int    `json:"status,omitempty"`
	Detail     string `json:"detail,omitempty"`
	ResponseTo string `json:"responseTo,omitempty"`
}

// FeatureOptions is the programmatic input to RunFeature — the same
// fields that the CLI's --issue, --attempt, --probes flags carry.
type FeatureOptions struct {
	// Probes from the issue's acceptance block. Empty = healthz-only smoke.
	Probes []Probe
	// Issue + Attempt are used to name the screenshot file
	// (screenshots/S<Issue>-attempt-<Attempt>.png).
	Issue, Attempt int
}

// RunFeature is the programmatic entry point for the feature check.
// The CLI wrapper Feature() parses flags, builds a FeatureOptions, and
// delegates here. This split is what lets loop.Work invoke the check
// in-process instead of shelling out to its own binary (thermo S5).
func RunFeature(cwd string, cfg *config.Config, opts FeatureOptions) (*FeatureReport, error) {
	if err := cfg.Validate(config.RequireProjectRepo, config.RequireFeatureBinary); err != nil {
		return nil, err
	}

	ui.Header("feature check")
	ui.Step("build (%d step%s)", len(cfg.FeatureBuild), plural(len(cfg.FeatureBuild)))
	buildLog, err := runBuild(cwd, cfg.FeatureBuild)
	writeBuildLog(cwd, cfg, buildLog)
	if err != nil {
		return nil, fmt.Errorf("build failed:\n%s", truncate(buildLog, 4000))
	}

	// 2. Launch.
	binAbs := filepath.Join(cwd, cfg.FeatureBinary)
	if _, err := os.Stat(binAbs); err != nil {
		return nil, fmt.Errorf("binary missing after build: %s", binAbs)
	}
	envKV := strings.SplitN(cfg.FeatureEnableEnv, "=", 2)
	if len(envKV) != 2 {
		return nil, fmt.Errorf("feature_test.enable_env must be NAME=VALUE, got %q", cfg.FeatureEnableEnv)
	}
	_ = os.Remove(cfg.FeaturePortFile)

	cmd := exec.Command(binAbs)
	cmd.Env = append(os.Environ(), envKV[0]+"="+envKV[1])
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("launch %s: %w", binAbs, err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(os.Interrupt)
			time.AfterFunc(2*time.Second, func() {
				_ = cmd.Process.Kill()
			})
			_ = cmd.Wait()
		}
	}()

	port, err := waitForPort(cfg.FeaturePortFile, time.Duration(maxInt(cfg.FeatureHealthzTimeoutS, 10))*time.Second)
	if err != nil {
		return nil, fmt.Errorf("port file %s did not appear: %v\nstderr:\n%s", cfg.FeaturePortFile, err, truncate(stderr.String(), 2000))
	}
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	ui.OK("app up on %s", baseURL)

	if err := waitForHealthz(baseURL+pathOr(cfg.FeatureHealthzEndpoint, "/healthz"), 5*time.Second); err != nil {
		return nil, fmt.Errorf("/healthz never went green: %v\nstderr:\n%s", err, truncate(stderr.String(), 2000))
	}

	report := FeatureReport{
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		BuildLog:  truncate(buildLog, 8000),
	}
	allOK := true
	for _, p := range opts.Probes {
		for _, c := range p.Calls {
			ok, status, detail, _ := callOne(baseURL, c)
			report.Steps = append(report.Steps, StepRes{
				Step:       p.Step,
				OK:         ok,
				Status:     status,
				Detail:     detail,
				ResponseTo: c.Method + " " + c.Path,
			})
			if ok {
				ui.OK("%s %s", c.Method, c.Path)
			} else {
				ui.Fail("%s %s — %s", c.Method, c.Path, detail)
				allOK = false
			}
		}
	}
	report.Pass = allOK
	report.FinishedAt = time.Now().UTC().Format(time.RFC3339)

	// Screenshot — best-effort, never fails the check.
	if path, err := captureScreenshot(baseURL, cwd, opts.Issue, opts.Attempt); err != nil {
		ui.Warn("screenshot: %v", err)
	} else if path != "" {
		report.ScreenshotPath = path
		ui.OK("screenshot saved → %s", path)
	}

	// Graceful shutdown.
	_ = postJSON(baseURL+pathOr(cfg.FeatureShutdownEndpoint, "/shutdown"), nil, 2*time.Second)

	if err := writeReport(cwd, cfg, &report); err != nil {
		return &report, err
	}
	if !report.Pass {
		return &report, fmt.Errorf("feature check failed")
	}
	ui.OK("feature check: PASS")
	return &report, nil
}

// Feature is the CLI wrapper: parses --probes, --issue, --attempt,
// --config, and calls RunFeature.
func Feature(args []string) error {
	fs := flag.NewFlagSet("feature", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to .agent/config.yaml")
	probesPath := fs.String("probes", "", "JSON file with acceptance probes")
	issueNum := fs.Int("issue", 0, "issue number (used in screenshot filename)")
	attempt := fs.Int("attempt", 0, "attempt number (used in screenshot filename)")
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

	opts := FeatureOptions{Issue: *issueNum, Attempt: *attempt}
	if *probesPath != "" {
		b, err := os.ReadFile(*probesPath)
		if err != nil {
			return err
		}
		var acc Acceptance
		if err := json.Unmarshal(b, &acc); err != nil {
			return fmt.Errorf("parse probes file %s: %w", *probesPath, err)
		}
		opts.Probes = acc.Acceptance
	}

	_, err = RunFeature(cwd, cfg, opts)
	return err
}

func runBuild(cwd string, steps []string) (string, error) {
	var log strings.Builder
	for _, step := range steps {
		log.WriteString("$ " + step + "\n")
		parts := strings.Fields(step)
		if len(parts) == 0 {
			continue
		}
		r, err := sh.Run(cwd, parts[0], parts[1:]...)
		log.WriteString(r.Combined())
		if err != nil {
			return log.String(), err
		}
		if r.ExitCode != 0 {
			return log.String(), fmt.Errorf("%q exited %d", step, r.ExitCode)
		}
	}
	return log.String(), nil
}

func writeBuildLog(cwd string, cfg *config.Config, log string) {
	stateDir := cfg.StateDir
	if stateDir == "" {
		stateDir = ".slate"
	}
	dir := filepath.Join(cwd, stateDir, "checks")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "build.log"), []byte(log), 0o644)
}

func writeReport(cwd string, cfg *config.Config, r *FeatureReport) error {
	stateDir := cfg.StateDir
	if stateDir == "" {
		stateDir = ".slate"
	}
	dir := filepath.Join(cwd, stateDir, "checks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "feature.json"), append(b, '\n'), 0o644)
}

func waitForPort(path string, timeout time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			s := strings.TrimSpace(string(b))
			var n int
			if _, err := fmt.Sscanf(s, "%d", &n); err == nil && n > 0 {
				return n, nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return 0, fmt.Errorf("timeout after %s", timeout)
}

func waitForHealthz(url string, timeout time.Duration) error {
	client := &http.Client{Timeout: 1 * time.Second}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(150 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timeout after %s", timeout)
	}
	return lastErr
}

func callOne(base string, c Call) (ok bool, status int, detail string, body []byte) {
	client := &http.Client{Timeout: 5 * time.Second}
	url := base + c.Path

	var req *http.Request
	var err error
	if len(c.Body) > 0 {
		req, err = http.NewRequest(strings.ToUpper(c.Method), url, bytes.NewReader([]byte(c.Body)))
		if req != nil {
			req.Header.Set("Content-Type", "application/json")
		}
	} else {
		req, err = http.NewRequest(strings.ToUpper(c.Method), url, nil)
	}
	if err != nil {
		return false, 0, "build request: " + err.Error(), nil
	}

	resp, err := client.Do(req)
	if err != nil {
		return false, 0, "http: " + err.Error(), nil
	}
	defer resp.Body.Close()
	body, _ = io.ReadAll(resp.Body)
	status = resp.StatusCode

	if status < 200 || status >= 300 {
		return false, status, fmt.Sprintf("non-2xx %d body=%s", status, truncate(string(body), 500)), body
	}

	if len(c.Expect) == 0 {
		return true, status, "", body
	}

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		return false, status, "response not JSON: " + err.Error() + " body=" + truncate(string(body), 200), body
	}
	var want map[string]any
	if err := json.Unmarshal(c.Expect, &want); err != nil {
		return false, status, "expect not JSON: " + err.Error(), body
	}
	for k, v := range want {
		gv, ok := got[k]
		if !ok {
			return false, status, fmt.Sprintf("missing key %q in response", k), body
		}
		if !deepEqualJSON(gv, v) {
			return false, status, fmt.Sprintf("key %q: got %v want %v", k, gv, v), body
		}
	}
	return true, status, "", body
}

func deepEqualJSON(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

// captureScreenshot fetches GET /screenshot and writes the PNG body to
// screenshots/S<issue>-attempt-<attempt>.png under cwd. Returns the
// repo-relative path on success. Empty path on quiet success-without-issue
// (e.g. issue=0 meaning the orchestrator didn't pin a number, so we
// fall back to a timestamped name).
func captureScreenshot(baseURL, cwd string, issue, attempt int) (string, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(baseURL + "/screenshot")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("/screenshot returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if len(body) < 8 || string(body[:8]) != "\x89PNG\r\n\x1a\n" {
		return "", fmt.Errorf("/screenshot did not return PNG (got %d bytes, leading: %q)", len(body), truncate(string(body), 80))
	}

	dir := filepath.Join(cwd, "screenshots")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	var name string
	if issue > 0 {
		name = fmt.Sprintf("S%d-attempt-%d.png", issue, attempt)
	} else {
		name = fmt.Sprintf("ad-hoc-%d.png", time.Now().Unix())
	}
	full := filepath.Join(dir, name)
	if err := os.WriteFile(full, body, 0o644); err != nil {
		return "", err
	}
	return filepath.Join("screenshots", name), nil
}

func postJSON(url string, body []byte, timeout time.Duration) error {
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(truncated)"
}

func pathOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// Quality runs the code-quality agent (read-only pi invocation) against
// the latest commit. Files blocking comments on the PR (if any open).
//
// TODO(007-builder): implement after the first end-to-end Work cycle runs.
func Quality(args []string) error {
	return fmt.Errorf("TODO: check quality not yet implemented")
}
