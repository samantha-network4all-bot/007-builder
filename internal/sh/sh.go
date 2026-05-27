// Package sh wraps os/exec with the conveniences the orchestrator
// uses everywhere: cwd-scoped commands, combined output capture,
// and explicit exit-code separation.
package sh

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
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

// Stream is like Run but pipes stdout and stderr line-by-line to the
// given callbacks AS they arrive (no buffering of the whole output
// before returning). Useful for long-running LLM invocations where the
// user wants to watch progress instead of waiting in silence.
//
// Stream also accumulates the full output into Result.Stdout/.Stderr,
// so callers that want both live + retained output get it for free.
//
// Lines are split on \n. A trailing line without a newline is also
// delivered. Buffer is sized for pi's JSON event lines (which can be
// dozens of KB when they carry deltas).
func Stream(dir, name string, args []string, onStdout, onStderr func(string)) (Result, error) {
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	full := name + " " + strings.Join(args, " ")
	res := Result{Cmd: full}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return res, fmt.Errorf("stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return res, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return res, fmt.Errorf("start %s: %w", full, err)
	}

	var (
		stdoutBuf, stderrBuf bytes.Buffer
		wg                   sync.WaitGroup
	)
	wg.Add(2)
	go pump(&wg, stdoutPipe, &stdoutBuf, onStdout)
	go pump(&wg, stderrPipe, &stderrBuf, onStderr)
	wg.Wait()

	err = cmd.Wait()
	res.Stdout = stdoutBuf.String()
	res.Stderr = stderrBuf.String()
	if err == nil {
		res.ExitCode = 0
		return res, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.ExitCode = ee.ExitCode()
		return res, nil
	}
	return res, fmt.Errorf("exec %s: %w", full, err)
}

// pump scans a pipe line-by-line, tees to the buffer, and invokes the
// per-line callback. Tolerates very long lines (pi event JSON can be 64K+).
func pump(wg *sync.WaitGroup, r io.Reader, buf *bytes.Buffer, onLine func(string)) {
	defer wg.Done()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		buf.WriteString(line)
		buf.WriteByte('\n')
		if onLine != nil {
			onLine(line)
		}
	}
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
