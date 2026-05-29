# Lessons learned from the `notepad/` repo (May 2026)

This document collects everything the previous attempt at building a
Win10-Notepad clone taught us. Every defect here is a real bug that
the prior automation either shipped or failed to detect. The PRD's
§8 "Architectural invariants" derives from this list — treat the two
documents as the same content viewed from different angles.

## Part 1 — What the *app* taught us

### 1.1 `@main` on an `NSApplicationDelegate` doesn't bootstrap AppKit
- **Symptom**: app launched, exited zero, no window ever appeared.
  Activity Monitor showed no Notepad process.
- **Root cause**: `@main` on an `NSObject` does not synthesise
  `NSApplicationMain` — only SwiftUI's `App` protocol does. So
  `applicationDidFinishLaunching` was never called.
- **Invariant**: every __PROJECT_NAME__ target ships an explicit `main.swift`
  that creates `NSApplication.shared`, sets `setActivationPolicy(.regular)`,
  installs the delegate, and calls `app.run()`. No `@main` on AppDelegate.

### 1.2 `NSTextView` with an orphan text container has no backing store
- **Symptom**: editor rendered, focus indicator visible, but typing
  produced no visible text. The Accessibility API even reported the
  typed characters via `AXValue` — they were stored *somewhere* but
  not displayed, and `NSText.didChange` never fired.
- **Root cause**: the text container was constructed with
  `NSTextContainer()` and handed to `NSTextView(frame:textContainer:)`
  without a layout manager or text storage attached.
- **Invariant**: every `NSTextView` in __PROJECT_NAME__ is built by the canonical
  chain `NSTextStorage → NSLayoutManager → NSTextContainer → NSTextView`.
  PRD §8 makes orphan containers a build-blocker for code review.

### 1.3 First-responder set to the scroll view instead of the editor
- **Symptom**: even after fixing §1.2, typing still didn't work.
- **Root cause**: `window.makeFirstResponder(editorScrollView)` —
  the scroller doesn't forward key events to the text view inside it.
- **Invariant**: `makeFirstResponder` must always receive the inner
  text view (`scrollView.documentView` or a direct reference), never
  the scroller.

### 1.4 `NSImage(imageLiteralResourceName:)` traps on missing assets
- **Symptom**: opening the File dialog crashed the app with
  `EXC_BREAKPOINT` (asset-not-found trap).
- **Root cause**: that initializer is for `#imageLiteral` and is
  non-failable. The code referenced `"NSTouchBarFolderIcon"` which
  doesn't exist on every macOS configuration.
- **Invariant**: image loading uses failable `NSImage(named:)` with
  an explicit fallback (`NSImage.folderName`, blank `NSImage(size:)`,
  etc.). PRD §8 forbids `imageLiteralResourceName`.

### 1.5 Reflexive sidebar callback recursion
- **Symptom**: opening File→Open eventually crashed with `SIGSEGV`
  from stack overflow.
- **Root cause**: `FileBrowserSidebar.selectDirectory(at:)` called
  `selectItem(_:)` which fired `onDirectorySelected` which called
  back into `FileBrowserDialog.navigate(to:)` which called
  `loadDirectory(at:)` which called `selectDirectory(at:)` again.
- **Invariant**: any method whose name implies "make X the current
  selection / location" must update visual state only and MUST NOT
  emit the user-action callback that other code uses to *request*
  the same selection. PRD §8 codifies this as a hard rule.

### 1.6 Borderless windows must override `canBecomeKey` / `canBecomeMain`
- **Symptom**: subtle — the window appeared but didn't accept clicks
  reliably; menu items dispatched through the responder chain were
  flaky.
- **Root cause**: `NSWindow` subclass with `[.borderless, …]` style
  mask returns `false` from `canBecomeKey` by default.
- **Invariant**: every borderless window class overrides both
  `canBecomeKey` and `canBecomeMain` to `true`.

### 1.7 Custom file dialogs are a tar pit
- **Symptom**: the in-house File→Open dialog crashed (§1.4), recursed
  (§1.5), and hung the app even after both were fixed because its
  modality model (floating window vs. sheet vs. NSApp.runModal) was
  never settled.
- **Root cause**: trying to recreate the Win10 file dialog as a first
  feature, instead of getting the *file open* flow working at all.
- **Invariant**: __PROJECT_NAME__ ships `NSOpenPanel` / `NSSavePanel` first.
  A custom Win10 dialog becomes its own late-stage `S`-issue with
  its own acceptance tests, never bundled with "open a file."

## Part 2 — What the *automation* taught us

### 2.1 "Window exists" ≠ "app works"
- **Symptom**: every codecheck iteration declared "no defects" while
  the app was unusable.
- **Root cause**: `quality-check.sh` verified `{process alive,
  ≥1 window owned by pid, stderr free of crash signatures}`. None of
  that proves the editor accepts typing or that menu commands work.
- **Invariant**: in __PROJECT_NAME__ the build check MUST invoke at least one
  real product behavior end-to-end (write text via `/type`, read it
  back via `/text`). A green check that didn't exercise a feature
  isn't a green check.

### 2.2 `osascript`-based input silently degrades
- **Symptom**: the `-i` rung typed a known string with synthetic
  keystrokes and reported `skipped-no-ax` whenever Accessibility
  wasn't granted to Terminal. The loop's "when to write empty" rule
  treated `skipped` as not-failing, so it kept declaring the app
  fine.
- **Root cause**: synthetic input has two failure modes (system
  blocks the call vs. the call succeeds but does nothing observable),
  and they look identical to the harness.
- **Invariant**: __PROJECT_NAME__'s feature checks talk to the app via
  `127.0.0.1:<port>` HTTP. `osascript` and `CGEvent` are banned from
  the orchestrator. Permissions don't degrade HTTP.

### 2.3 `grep -c || echo 0` returns `"0\n0"`
- **Symptom**: integer comparison `if [ "$N" -gt 0 ]` exploded with
  syntax error on inputs that should have been just `0`.
- **Root cause**: `grep -c` already exits non-zero when no matches
  found, but it still *prints* `0`. The `|| echo 0` then appends a
  second `0`, producing `"0\n0"`.
- **Invariant**: no shell command substitution chains in the
  orchestrator. Counters are produced by Go code with typed
  integers.

### 2.4 `.gitignore` inline comments break patterns
- **Symptom**: `Notepad.xcodeproj/` was tracked in git even though
  `.gitignore` had a line for it.
- **Root cause**: the line was
  `Notepad.xcodeproj/       # generated by xcodegen`. `#` is only
  a comment marker at the *start* of a line. With trailing chars
  the whole string becomes a literal pattern with whitespace.
- **Invariant**: every `.gitignore` line is either fully a comment
  or fully a pattern. The orchestrator's bootstrap writes
  `.gitignore` with this rule baked in.

### 2.5 Heredoc `$0` collides with Swift closure shorthand
- **Symptom**: a Swift snippet embedded in a bash heredoc had its
  `$0` (the closure's first argument) substituted with the bash
  script's path. Compilation failed weirdly.
- **Root cause**: heredoc without `<<'EOF'` (quoted) interpolates
  `$VAR` substitutions, including positional ones.
- **Invariant**: the orchestrator's templates are Go `text/template`,
  not heredocs. Swift literals never round-trip through bash.

### 2.6 "missing" worker status hid LLM connection-refused
- **Symptom**: `ralph-parallel.sh` reported workers as "missing" —
  no status file written. Hours wasted assuming the workers ran but
  forgot to write the file.
- **Root cause**: the LLM upstream (ollama) was unreachable; `pi`
  exited fast, never created the file. The wrapper conflated "didn't
  run" with "ran and crashed silently".
- **Invariant**: each agent invocation in __PROJECT_NAME__ is wrapped by Go
  code that distinguishes exit-code categories: LLM-unreachable,
  agent-refused-task, agent-wrote-bad-output, agent-succeeded.
  These map to four different next-actions, not one "missing".

### 2.7 `pi`/`claude` CLI exits aren't self-describing
- **Symptom**: agents would exit 0 even when they accomplished
  nothing.
- **Root cause**: the CLIs treat "model returned text" as success.
  Whether that text actually edited files is a separate question.
- **Invariant**: every code-agent invocation in __PROJECT_NAME__ is sandwiched
  by `git rev-parse HEAD` before/after. If HEAD didn't move, the
  attempt is recorded as `agent-noop`, not as a success.

### 2.8 Skipping the build is the most common false success
- **Symptom**: code-quality reviews approved diffs that didn't
  compile.
- **Root cause**: review prompt didn't require seeing build output.
- **Invariant**: in __PROJECT_NAME__, no quality review starts until the
  feature-test check has already run `xcodegen` + `xcodebuild`
  cleanly. Quality review reads the build log as input.

## Part 3 — Process scars (kept here so the orchestrator doesn't relearn them)

- **Don't ship features as part of "open a file"**. Six smaller
  slices beat one all-encompassing one. (See §1.7 + the custom
  dialog disaster.)
- **An issue without an HTTP probe is not a slice.** If you can't
  describe how to test it from `curl`, you can't test it at all
  on a headless macOS box. Reject the issue, ask for a probe.
- **Always commit on a working tree.** The old loop refused to
  start on a dirty tree but didn't enforce a clean tree *after*
  its own work — abandoned `.swift` half-edits accumulated.
  __PROJECT_NAME__'s orchestrator `git status --porcelain` checks both
  pre- and post-agent.
- **Never push to a branch other than `main` from a loop.** HITL
  handoff uses `review/<N>` branches, but those are PR-targets,
  not push targets for autonomous work. The old codebase honored
  this; keep honoring it.
