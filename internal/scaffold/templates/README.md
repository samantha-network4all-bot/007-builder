# __PROJECT_NAME__

Agent-built via [007-builder](https://github.com/samantha-network4all-bot/007-builder).

This repository was scaffolded by `builder init` from the slate example.
The application is built iteratively, one vertical slice at a time, by an
LLM code agent driven by the orchestrator.

## How it works

- **`PRD.md`** — the product contract. Read §7 (HTTP test API) and §8
  (architectural invariants) before touching code; the orchestrator's
  quality review enforces them mechanically.
- **`lessons-learned.md`** — historical defects to avoid.
- **`.agent/`** — orchestrator config, prompt templates, and skills.
- **`Project.yml`** — XcodeGen project definition. The `.xcodeproj` is
  generated on every build and is git-ignored.

## Building locally

```sh
xcodegen generate
xcodebuild -scheme __PROJECT_NAME__ -configuration Debug -derivedDataPath build/ build
```

## Driving the loop

```sh
builder loop --caveman        # next-issue → work, repeated until done
```

Every feature is reachable from the localhost HTTP test API (enabled via
`__PROJECT_NAME_UPPER___TEST_API=1`), so progress is verified headlessly
with HTTP probes and screenshots rather than manual clicking.
