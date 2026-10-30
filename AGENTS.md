# CPA Plus Plugin

## Non-negotiables

The Principles section below grounds every trigger. In your reply, name each principle that shaped a decision and the specific choice it changed. Cite only principles whose leaf SKILL.md you read this session.

Remaining triggers:

- Any prose surface, your reply to the user included → the **unslop** skill.
- Before commit → the **deslop** skill.

## Principles

Read the leaf skill in full for any principle you apply. Each entry names when it applies. Each leaf is the **principle-<title>** skill, with the title in kebab case, hyphens kept and other punctuation dropped, unless the entry gives its name.

### Core

- **Laziness Protocol**. Refactoring, sizing a diff, or tempted to add abstractions, layers, or signal threading. Bias to deletion and the smallest change that solves the problem.

- **Foundational Thinking**. Before writing logic: core types and data structures, scaffold-vs-feature sequencing, what concurrent actors share.

- **Redesign from First Principles**. Integrating a new requirement into an existing design. Redesign as if it had been foundational from day one.

- **Subtract Before You Add**. Sequencing an addition, refactor, or rewrite. Remove dead weight first, then build on the simpler base.

- **Minimize Reader Load**. Reviewing or shaping code that's hard to trace. Count layers and hidden state, collapse one-caller wrappers, shrink mutable scope.

- **Outcome-Oriented Execution**. Planned rewrites and migrations with explicit phase boundaries. Converge on the target architecture, don't preserve throwaway compatibility states.

- **Experience First**. Product, UX, or feature-scope tradeoffs. Choose user delight over implementation convenience.

### Architecture

- **Model the Domain**. Writing stateful logic, or code that branches a lot or repeats a shape assumption across files. Encode the domain in a structure (state machine, typed model, table or registry, reducer, boundary, the right collection) instead of scattered conditionals.

- **Boundary Discipline**. Wiring validation, error handling, or framework adapters. Guards at system boundaries, trust internal types, keep business logic pure.

- **Type System Discipline**. Designing types or a signature in any typed language. Make illegal states unrepresentable, brand primitives, parse external data at boundaries.

- **Make Operations Idempotent**. Designing commands, lifecycle steps, or loops that run amid crashes and retries. Converge to the same end state.

- **Migrate Callers Then Delete Legacy APIs**. Introducing a new internal API while old callers exist. Migrate and delete in one wave.

- **Separate Before Serializing Shared State**. Concurrent actors might write the same file, branch, key, or object. Eliminate the sharing first.

### Verification

- **Fix Root Causes**. Debugging. Trace each symptom back to the first place the system went wrong and fix it there. A guard is a fix only when the state it handles is legitimate.

- **Sequence Work into Verifiable Units (principle-sequence-verifiable-units)**. Multi-step work (sweeps, migrations, runs of similar edits) and how you stack commits and PRs. Break work into small units that each end in a check, verify each before the next, and order delivery so the sequence proves itself.

- **Test Behavior, Not Implementation**. Writing, changing, or keeping a test. Assert what the code's users can observe, so the test fails when that behavior breaks and keeps passing when only the internals change.

### Delegation

- **Guard the Context Window**. Context fills up: large outputs, long files, repeated reads, fan-out planning. Route bulk to subagents, keep summaries in the main thread.

- **Never Block on the Human**. Tempted to ask "should I do X?" on reversible work. Proceed, present the result, let the human course-correct.

### Meta

- **Encode Lessons in Structure**. You catch yourself writing the same instruction a second time. Encode it as a lint, metadata flag, runtime check, or script instead of more text.

## Pull requests

- Never make a PR unless the developer explicitly asks you to do so.
- Conventional commit titles, plain language: `fix(web): new threads no longer spike CPU`.
- Body: the problem in a sentence or two, then how you fixed it. End with the model and harness that did the work.
- UI changes need before/after images.
- Upload PR evidence to GitHub. Never commit PR-only screenshots or other PR-only assets into the repo.
- When babysitting: call `watch_pull_request` and end your turn. T3 Code wakes you on checks, comments, or conflicts. Do not poll or sleep. Verify each bot finding against the source, fix real ones, dismiss false positives with a written reason. Stay quiet when nothing is new. Stop when the bots are green on the latest commit.

## Taste

- Do not add comments to Go code.
- Never publish a new version unless the developer explicitly asks you to do so.
