// Package sh wraps os/exec with the conveniences the orchestrator
// uses everywhere: cwd-scoped commands, combined output capture,
// and explicit exit-code separation.
package sh

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Result is the structured outcome of one process invocation.
type Result struct {
	Cmd      string // the command line, for logging
	Stdout   string
	Stderr   string
	ExitCode int
}

// Combined returns stdout + stderr glued together — handy for human-facing logs.
func (r Result) Combined() string {
	return r.Stdout + r.Stderr
}

// Run executes name+args in the given working directory. dir may be ""
// to inherit the caller's cwd. Returns Result regardless of success;
// the error is non-nil only for "the process did not run" failures
// (binary not found, etc.) — a non-zero exit code is reflected in
// Result.ExitCode, not in error. Use Run + Result.ExitCode to branch.
func Run(dir, name string, args ...string) (Result, error) {
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	full := name + " " + strings.Join(args, " ")
	err := cmd.Run()
	res := Result{
		Cmd:    full,
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}
	if err == nil {
		res.ExitCode = 0
		return res, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.ExitCode = ee.ExitCode()
		return res, nil
	}
	// process-did-not-run failure
	return res, fmt.Errorf("exec %s: %w", full, err)
}

// MustRun is Run that treats non-zero exit as a returned error. Use
// when the caller would always wrap a non-zero exit as a fatal anyway.
func MustRun(dir, name string, args ...string) (Result, error) {
	r, err := Run(dir, name, args...)
	if err != nil {
		return r, err
	}
	if r.ExitCode != 0 {
		return r, fmt.Errorf("%s exited %d\nstderr:\n%s", r.Cmd, r.ExitCode, r.Stderr)
	}
	return r, nil
}
