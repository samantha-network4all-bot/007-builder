// Package ui is the orchestrator's tiny logging surface. Output is
// ANSI-coloured when stdout is a TTY, plain otherwise — so piped logs
// and CI captures stay readable.
package ui

import (
	"fmt"
	"os"
	"strings"
	"time"
)

var color = isTerminal(os.Stdout)

// Disable forces colors off. Test code calls this.
func Disable() { color = false }

const (
	reset    = "\033[0m"
	dim      = "\033[2m"
	bold     = "\033[1m"
	red      = "\033[31m"
	green    = "\033[32m"
	yellow   = "\033[33m"
	blue     = "\033[34m"
	magenta  = "\033[35m"
	cyan     = "\033[36m"
	gray     = "\033[90m"
)

func wrap(c, s string) string {
	if !color {
		return s
	}
	return c + s + reset
}

func ts() string { return wrap(gray, time.Now().Format("15:04:05")+" ") }

// Step prints a "in progress" line with a leading arrow.
func Step(format string, a ...any) {
	fmt.Println(ts() + wrap(cyan, "▸ ") + fmt.Sprintf(format, a...))
}

// OK prints a green checkmark line.
func OK(format string, a ...any) {
	fmt.Println(ts() + wrap(green, "✓ ") + fmt.Sprintf(format, a...))
}

// Fail prints a red cross line.
func Fail(format string, a ...any) {
	fmt.Println(ts() + wrap(red, "✗ ") + fmt.Sprintf(format, a...))
}

// Warn prints a yellow caution line.
func Warn(format string, a ...any) {
	fmt.Println(ts() + wrap(yellow, "! ") + fmt.Sprintf(format, a...))
}

// Note prints a dim informational line.
func Note(format string, a ...any) {
	fmt.Println(ts() + wrap(gray, "· "+fmt.Sprintf(format, a...)))
}

// Header prints a bold magenta section header (visual breath between
// phases — bootstrap, next-issue, work, etc.).
func Header(format string, a ...any) {
	bar := strings.Repeat("─", 4)
	line := fmt.Sprintf(format, a...)
	if color {
		fmt.Println()
		fmt.Println(wrap(magenta+bold, "═══ "+line+" ") + wrap(magenta, bar))
	} else {
		fmt.Println()
		fmt.Println("=== " + line + " ===")
	}
}

// Issue prints an "issue #N: title" line in blue.
func Issue(n int, title string) {
	fmt.Println(ts() + wrap(blue+bold, fmt.Sprintf("#%d ", n)) + title)
}

// KV prints a key=value pair, dim key.
func KV(k, v string) {
	fmt.Println(ts() + wrap(gray, k+": ") + v)
}

// Outcome prints the LLM outcome line with a colour matching its
// severity.
func Outcome(name, detail string) {
	var c string
	switch strings.ToLower(name) {
	case "succeeded":
		c = green
	case "noop":
		c = yellow
	case "bad-output", "refused":
		c = red
	case "unreachable":
		c = magenta
	default:
		c = gray
	}
	fmt.Println(ts() + wrap(c+bold, name) + " " + wrap(gray, detail))
}

// isTerminal returns true when fd is a TTY. We avoid pulling in
// golang.org/x/term — checking the IsCharDevice mode bit is enough.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}
