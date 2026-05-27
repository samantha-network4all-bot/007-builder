// Package loop owns the high-level orchestrator commands: bootstrap,
// work, and run (the full iterative loop). The per-issue state machine
// also lives here.
package loop

import "fmt"

// Bootstrap creates the GitHub repo, writes the seed files, and pushes
// the first commit.
//
// TODO(slate-v2): implement.
//
// Sketch:
//   1. Validate gh CLI is available and authenticated.
//   2. `gh repo create samantha-network4all-bot/slate-v2 --public --confirm` (idempotent: check first).
//   3. Write embedded templates (Project.yml, Slate/main.swift, Slate/AppDelegate.swift,
//      Slate/Info.plist, Slate/TestAPI/TestAPIServer.swift with /healthz only) into
//      the current working directory.
//   4. `git init && git add -A && git commit -m "S0: seed scaffold" && git push -u origin main`.
//   5. Open issue S1 ("Empty window appears at launch") with acceptance probes
//      for GET /healthz and GET /windows.
func Bootstrap(args []string) error {
	return fmt.Errorf("TODO: bootstrap not yet implemented (see %s comment)", "loop.Bootstrap")
}

// Work picks the oldest open `slice` issue, invokes the code agent, runs the
// two checks, and either closes the issue (green) or increments attempt:N
// (red, with N<10) or hands off to a human (N==10).
//
// TODO(slate-v2): implement.
func Work(args []string) error {
	return fmt.Errorf("TODO: work not yet implemented (see %s comment)", "loop.Work")
}

// Run repeats next-issue + work until next-issue says "PRD complete".
//
// TODO(slate-v2): implement.
func Run(args []string) error {
	return fmt.Errorf("TODO: loop not yet implemented (see %s comment)", "loop.Run")
}
