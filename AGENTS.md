# Agent Instructions

## Repository Role And Authority

This repository is the complete source for `awa`: the binary, its embedded
documentation, its executable tests, the public site, and the build and release
inputs. No other repository is needed to build, test, run, document, or release it.
`README.md` names the tools a checkout expects.

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
- When you author a commit, end its message with exactly these two trailers,
  written explicitly:

  ```text
  Co-authored-by: Codex <noreply@openai.com>
  Co-authored-by: Claude <noreply@anthropic.com>
  ```

  The tracked `.claude/settings.json` only turns off Claude's automatic trailer
  so it cannot add a competing one. The check itself stays conditional: human
  and bot commits that declare no co-author still pass.
- Treat `README.md` as current executable/user guidance and `justfile` as the
  canonical command surface; `just --list` shows the recurring workflows, and
  focused one-step work uses the native Go tool the README names. Site-specific
  mechanics live in `site/README.md`. Use the owning generation/update recipe
  instead of editing generated artifacts by hand.
- Run the checks the task requires and report every required check not run.
- Keep credentials, tokens, private endpoints, and local evidence out of source,
  logs, fixtures, and completion reports.

## Using awa

awa is optional here. A command wrapped by `awa run` also runs as the plain Go or
Just command on its own; without an installed `awa`, drop the wrapper, skip the
checkpoint and evidence steps, and keep the Git review and the check itself.
The installed binary's help describes that exact version (`awa help agents`,
`awa help topics`).

Read the dashboard before a review, then anchor the work to a checkpoint and keep
the full id it prints:

```bash
awa status
awa checkpoint -m "before <task>"
awa changes <checkpoint-id>..now
awa diff <checkpoint-id>..now
```

`latest` is shared local state and moves when anyone else checkpoints; an explicit
id does not. A checkpoint delta is focused memory, not proof that all pending work
was reviewed: before acceptance, also read the complete `git status` and
`git diff`.

Verify with the narrowest check that covers the change. The native command stands
on its own:

```bash
go test ./internal/app/run -run '^TestCommandMatches$'
```

Wrap it when the output should be retained as inspectable evidence:

```bash
awa run --record --stdin null --display tail:200 -- go test ./internal/app/run -run '^TestCommandMatches$'
awa run show --last --tail 500
```

- **Record or cache.** `--record` always executes and never publishes a reusable
  hit: use it for builds, generators, formatters, and any check whose output is
  the evidence. Drop it (`awa run --stdin null -- just check`) only where a replayed
  result still supports the claim. A cache miss alone does not invalidate earlier
  evidence; `awa run explain -- <command>` names the reason, and a real input is
  never excluded to win a hit. A selection that ran no test is not proof.
- **Stdin.** A piped or redirected stdin, which is what an agent harness usually
  hands awa, reaches the command and makes the run non-reusable. `--stdin null` is
  only for commands that read no input; never use it to silence one that does.
- **Environment.** A wrapped command gets awa's baseline plus the names in
  `[run].env_allowlist` of the committed `awa.toml`; anything else is dropped
  without a message, so `FOO=1 awa run -- <command>` runs without `FOO`. A newly
  needed variable is a policy change to propose, not a local workaround.
  `awa config effective` shows what applies. `.awaignore`, not `.gitignore`, is
  scan policy.
- **Privacy.** `.awa/` is private local evidence and is never committed. Output
  and argv are stored verbatim, so wrapped commands must not print secrets or take
  them as arguments. Checkpoints cover untracked and gitignored files; a
  secret-bearing file needs its `.awaignore` or hash-only entry before it exists,
  because later exclusion does not remove stored bytes (`awa help privacy`).
- **Findings before repairs.** `awa doctor` (exit 5 means findings, not failure),
  `awa run audit`, `awa gc --dry-run`, and `awa restore <checkpoint-id> -- <path>`
  only inspect. `awa doctor --repair`, `awa gc`, `awa run rm`, `awa restore --apply`,
  and any config change delete or alter evidence or the worktree: propose them with
  their findings and leave the decision to the maintainer.
