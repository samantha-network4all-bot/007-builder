// Package tmpl is the one canonical home for prompt-template rendering
// in the orchestrator. Both the planner and the reviewer need to:
//
//  1. read a *.tmpl file from the project's prompt directory,
//  2. execute it as a text/template with a per-call data map,
//  3. drop the rendered string into a temp file so pi can consume it
//     via --append-system-prompt @file.
//
// Before extraction, plan.go and review.go each carried verbatim
// copies of these helpers — flagged by the thermo-nuclear sweep.
package tmpl

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

// Render reads the template at path, executes it against data, and
// returns the rendered string. Returns an error wrapping path on any
// I/O or template failure.
func Render(path string, data any) (string, error) {
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

// WriteTemp writes the given content to a uniquely-named temp file
// with the given prefix and returns its path. The caller is
// responsible for os.Remove on the returned path.
func WriteTemp(prefix, content string) (string, error) {
	f, err := os.CreateTemp("", prefix+"*.md")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// RenderToTemp combines Render + WriteTemp — the path pattern both
// the planner and reviewer follow. Returns the temp-file path; caller
// defers os.Remove.
func RenderToTemp(prefix, templatePath string, data any) (string, error) {
	rendered, err := Render(templatePath, data)
	if err != nil {
		return "", err
	}
	return WriteTemp(prefix, rendered)
}
