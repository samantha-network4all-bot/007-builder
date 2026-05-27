// builder is a project-agnostic agent-orchestration runner. It reads a
// per-project config (PRD path, prompt directory, test-API base URL) and
// drives an iterative code → check → review loop against a GitHub repo.
//
// See ../../README.md for the lifecycle and ../../examples/ for the
// Slate integration.
package main

import (
	"fmt"
	"os"

	"github.com/samantha-network4all-bot/007-builder/internal/checks"
	"github.com/samantha-network4all-bot/007-builder/internal/loop"
	"github.com/samantha-network4all-bot/007-builder/internal/plan"
)

const usage = `builder <command> [args]

Commands:
  bootstrap                 Create the GitHub repo + seed commit (one-shot).
  next-issue                Ask the LLM for the next slice; open a GitHub issue.
  work                      Pick the oldest open slice issue and run one
                            code-agent + check cycle.
  loop                      next-issue + work, repeated until the planner says done.
  check quality <issue>     Run the code-quality LLM review on the issue's PR.
  check feature <issue>     Build the target project and run the issue's HTTP probes.
  version                   Print version.

Config: builder reads ./.agent/config.yaml from cwd. Override with --config=PATH.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "bootstrap":
		err = loop.Bootstrap(args)
	case "next-issue":
		err = plan.NextIssue(args)
	case "work":
		err = loop.Work(args)
	case "loop":
		err = loop.Run(args)
	case "check":
		err = checks.Dispatch(args)
	case "version":
		fmt.Println("builder 0.1.0")
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %q\n\n%s", cmd, usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
