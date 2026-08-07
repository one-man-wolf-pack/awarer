# Agent Instructions

## Repository Role And Authority

This repository is the complete source for `awa`: the binary, its embedded
documentation, its executable tests, the public site, and the build and release
inputs. No other repository is needed to build, test, run, document, or release it.
`README.md` names the tools a checkout expects; `docs/releasing.md` and
`site/DEPLOYMENT.md` name the credentials publication needs.

Maintainer-directed work arrives with its own task-specific instructions. Follow
those for what to change and this file for how to work in the checkout; read what
they name rather than inferring project policy from the surrounding code. Route
contradictions, complexity stops, material implementation discoveries, and
genuine residual uncertainty back to the maintainer instead of deciding policy
locally.

## Working Here

- Stay inside the requested task. Preserve unrelated dirty work and never revert
  changes you did not make.
- Leave implementation changes uncommitted unless the user explicitly directs a
  commit.
- Commit co-author trailers are checked by `./scripts/check-co-authors.sh`; a
  commit declares no co-author or exactly the pair `README.md` names. The Validate
  workflow runs the check over every commit an event introduces and is the
  enforcement; `lefthook install` is optional local feedback and never a
  precondition for anything else here.
- Treat `README.md` as current executable/user guidance and `justfile` as the
  canonical command surface; `just --list` shows the recurring workflows, and
  focused one-step work uses the native Go tool the README names. Site-specific
  mechanics live in `site/README.md`. Use the owning generation/update recipe
  instead of editing generated artifacts by hand.
- Run the checks the task requires and report every required check not run.
- Keep credentials, tokens, private endpoints, and local evidence out of source,
  logs, fixtures, and completion reports.

## Using awa

Start with the installed binary; its help describes that exact version:

```bash
awa help agents
awa help privacy
awa status
```

Before risky or iterative work, create a checkpoint with a specific message and
keep the full id it prints:

```bash
awa checkpoint -m "before <task>"
awa changes <checkpoint-id>..now
awa diff <checkpoint-id>..now
```

`latest` is shared project-local state and may move when another user or agent
creates a checkpoint. Explicit ids keep a review/fix loop anchored. Checkpoint
deltas are focused memory, not proof that all pending work was reviewed: before
acceptance, also inspect the complete `git status` and Git diff.

Use awa to run deterministic validation and retain inspectable output:

```bash
awa run --display tail:200 -- just check
awa run --display tail:200 -- just stress
awa run show --last --tail 500
```

Discover reusable evidence and diagnose misses before repeating expensive work:

```bash
awa run ls
awa run ls --near
awa run explain -- just check
```

For builds, generators, formatters, migrations, deployment work, or any command
expected to mutate observed state, supervise it without publishing a cache hit:

```bash
awa run --record --display tail:200 -- just site
awa run show --last --tail 500
awa changes run:<id>:before..run:<id>:after
```

`awa run` is cwd-sensitive: run from the intended directory or use `--cwd`.
`--display` changes terminal rendering, not stored output or exit status.

When a build, generator, or formatter rewrites files it should not have, restore
just those paths from the checkpoint you were working against:

```bash
awa restore <checkpoint-id> -- internal/cli
awa restore --apply <checkpoint-id> -- internal/cli
```

Preview first and always name the paths. Preview is the default and writes
nothing; `--apply` is the only mutating form and requires an explicit selection
(paths, or `--all`); nothing outside the selection is touched; incomplete
evidence refuses with a named reason and there is no force option. An apply
records the state it overwrote and prints a `restore:<id>:before` reference to
undo it, retained under `[gc].keep_restores_for`. Running side-effecting commands
through `awa run --record` in the first place is what makes their before/after
evidence attributable — awa can observe drift but cannot prove which external
process caused it.

`.awa/` is private local evidence and must never be committed. Captured
stdout/stderr is stored verbatim and is not redacted, so wrapped commands must not
print secrets. Use `awa help privacy` and `awa doctor` when handling, moving, or
sharing the checkout.
