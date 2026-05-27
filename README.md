# 007-builder

A project-agnostic agent-orchestration runner. Given a PRD, a prompt
set, and a GitHub repo, `builder` drives an iterative
`pick slice → code → build → feature-test → quality-review` loop
until either the planner declares the spec covered or a per-issue
attempt cap forces a HITL handoff.

This tool grew out of the Ralph-style shell loops that built the
[Slate](https://github.com/samantha-network4all-bot/slate-v2) macOS
Notepad clone. The shell version's failure modes — silent
no-op agents, broken `grep -c` arithmetic, `.gitignore` inline-comment
bugs, false-positive QA harnesses — are catalogued in Slate's
`lessons-learned.md`. `007-builder` is the rewrite that prevents
them by construction: typed Go code, JSON-only inter-process IO,
HTTP-only feature verification, explicit categorisation of agent
exit modes.

## Status

Pre-alpha. The CLI dispatcher and module layout are in place;
every subcommand currently returns
`error: TODO: <name> not yet implemented`. Slice-by-slice
implementation is tracked in this repo's own GitHub issues.

## Install

```
git clone https://github.com/samantha-network4all-bot/007-builder.git
cd 007-builder
go build -o builder ./cmd/builder
```

## Usage from a consumer project

```
mycoolapp/
├── PRD.md                       # your product spec
├── .agent/
│   ├── config.yaml              # paths + LLM choice + repo name
│   └── prompts/
│       ├── PROMPT-code.tmpl
│       ├── PROMPT-quality.tmpl
│       └── PROMPT-next-issue.tmpl
└── (your sources)
```

```
cd mycoolapp
builder bootstrap                # creates the GitHub repo + seed commit
builder loop                     # iterative build until planner says done
```

The Slate integration lives in
[`samantha-network4all-bot/slate-v2`](https://github.com/samantha-network4all-bot/slate-v2)
under `.agent/`.

## Architecture

Single static binary. Subpackages:

- `internal/github` — wraps the `gh` CLI for issues, PRs, labels.
  Auth is inherited; no PATs in the binary.
- `internal/llm` — invokes `pi` (preferred) or `claude` CLIs.
  Categorises exits into Succeeded, Noop, BadOutput, Refused,
  Unreachable — the orchestrator branches differently for each.
- `internal/plan` — `next-issue`: renders the project's
  `PROMPT-next-issue.tmpl` with PRD + closed-slice JSON, asks the
  LLM, opens a GitHub issue with `slice` + `attempt:0` labels.
- `internal/checks` — `feature` (build the project, boot it with
  a project-defined env knob, curl its localhost HTTP probes) and
  `quality` (read-only LLM review of HEAD diff).
- `internal/loop` — per-issue state machine. On feature/quality
  failure, increments `attempt:N`. At configurable N (default 10),
  opens a PR and labels `awaiting-human-review`.

## Design principles

1. **Typed boundaries.** Every cross-process exchange (LLM output,
   feature-test probe result) is JSON validated by Go structs.
   No grep-and-pray.
2. **No synthetic UI input.** Feature verification flows through a
   localhost HTTP API that the target app exposes. macOS
   Accessibility permission, `osascript`, `CGEvent` — none of those
   touch the orchestrator.
3. **Distinguish agent exit modes.** "Connection refused to LLM"
   and "agent ran and committed nothing" produce different
   next-actions, not one ambiguous "missing".
4. **Pre-and-post tree-clean checks.** The runner refuses to start
   on a dirty tree and refuses to commit if an agent left
   uncommitted artefacts in unexpected paths.
5. **Reusable prompts, project-specific knowledge.** The runner
   owns the loop; the project owns the PRD, the prompt templates,
   and the test-API schema.

## License

TBD.
