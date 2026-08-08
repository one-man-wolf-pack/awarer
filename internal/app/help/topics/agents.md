# awa for coding agents

Use awa for three things: explicit checkpoints of the worktree, reusable
deterministic checks, and durable execution history. You do not need prior
knowledge of the project — the commands below are the whole workflow.

Start with the dashboard — it is the single entry point before a review:

```text
awa status                       # checkpoint, dirty summary, reusable/near
                                 # runs, and the commands to run next
```

Before a meaningful change, checkpoint the worktree (replace the placeholder
with a specific message; do not copy it literally):

```text
awa checkpoint -m "<specific-checkpoint-message>"
```

Review what changed since the last checkpoint (default range latest..now):

```text
awa changes
awa diff [path]
```

A clean changes/diff delta means only "no changes since that baseline" — not
that the whole worktree is reviewed. Before you declare work ready, inspect the
full current uncommitted worktree, not just the checkpoint delta.

`latest` is shared project-local state, not your private bookmark: it moves
whenever any agent or user runs `awa checkpoint`. For a long review/fix loop,
keep the checkpoint id that `awa checkpoint` prints and compare against it
explicitly so another agent's checkpoint cannot silently move your baseline:

```text
awa changes <checkpoint-id>..now
awa diff <checkpoint-id>..now
```

If a default `awa changes` baseline ever surprises you, inspect recent
checkpoints with `awa log -n 5`.

Run a deterministic check through the cache (record first, reuse if safe):

```text
awa run --display tail:200 -- <command>
awa run ls                       # checks reusable for the current state
awa run ls --near                # and why recent checks are now stale
awa run show --last --tail 500   # inspect the last run's captured output
awa run show --last --grep "<pattern>"
```

Run a non-cacheable or side-effecting command with supervised history, and other
useful forms:

```text
awa run --record --display tail:200 -- <command>
awa changes run:<id>:before..run:<id>:after   # what the command changed
awa run explain -- <command>     # show the cache decision without running
awa run --refresh -- <command>   # force a fresh deterministic check
```

## Repairing an accidental rewrite

A checkpoint is also evidence you can restore from — when a generator or
formatter rewrites files it should not have, put the selected paths back:

```text
awa restore <checkpoint-id> -- <path>            # preview; changes nothing
awa restore --apply <checkpoint-id> -- <path>    # perform it
```

Preview first and always name the paths: preview is the default and changes
nothing, `--apply` is the only mutating form and needs an explicit selection (paths
or `--all`), and nothing you did not select is touched. Incomplete evidence
refuses with a named reason; there is no force option. An apply records what it
overwrote and prints a `restore:<id>:before` reference to undo it, retained under
`[gc].keep_restores_for` — evidence with a lifetime, not a backup. See
[awa help restore](restore.md).

This is why side-effecting work belongs in `awa run --record`: a build,
generator, formatter, migration, or deployment run through the wrapper has its
before/after observation attributed immediately. awa can see a worktree drifted;
without the wrapper it cannot prove what caused it.

If awa notes that a large watched effect root (e.g. `target/`, `node_modules/`)
dominated state observation, that is why run, `run ls --near`, and status felt
slow — not a misconfiguration to fix by rerunning blindly. Prefer
`awa run --record` for a command whose point is a side effect; reviewing the
configured `[run]` effect roots is a policy decision, not a routine fix.

## Pick the right mode

- `awa checkpoint` — explicit, user-authored checkpoints.
- `awa run` — deterministic check: records first, becomes a reusable hit only
  if the run is clean, non-mutating, and its watched generated-output roots are
  unchanged.
- `awa run --record` — supervised history: always executes, never becomes a
  reusable hit. Use it for side-effecting, deployment, migration, and
  formatter/fixer workflows where replay would be misleading.
- `awa run ls` — the reusable-now view (reusable for the current observed
  inputs and configured effect-observation policy).
- `awa run log` and `awa run show` — execution history and captured-output
  inspection.

## Machine output

For automation, add `--json` — every review surface has a stable machine form
so you never parse human text:

```text
awa status --json                # dashboard facts + a review object
awa changes --json               # summary, refs, and per-file changes
awa run ls --near --json         # reusable rows + near misses (reason, sample)
awa run explain --json -- <command>
```

## Warnings are correctness signals, not noise

- Do not use cache mode for non-idempotent commands or commands that depend on
  hidden external state; a false hit would skip real work.
- A "skipped inputs" or "not cached" notice means the result was not made
  reusable — treat it as a signal, not clutter.
- `awa run` is cwd-sensitive by design: the execution directory is part of the
  cache key. Use `--cwd <path>` to make it explicit.
- `.gitignore` is OFF by default for awa's scan input, so a gitignored file is
  still a real input that can affect a command.

## Local evidence is private, not shared

- `.awa/` holds durable evidence (command lines, captured output, cwd,
  manifests, content blobs). Do not commit it — awa keeps it owner-private and
  untracked and restores its `.gitignore` guard on checkpoint/run.
- Allowlisted env values are keyed and passed to the child, but stored only as
  a redacted identity, never raw. Even so, do not print secrets through a
  wrapped command: captured stdout/stderr is stored verbatim as evidence.
- An executed child gets `AWA_RUN=1`. It is advisory and forgeable: it grants
  nothing and proves nothing.
- After a repo is moved, copied, or shared, run `awa doctor` to check the
  guard, permissions, and root hygiene.

## Discovering awa itself

Each of these answers for the version you have installed, without a network:

```text
awa version                            # the installed version
awa help topics                        # every operational topic
awa docs export --output <directory>   # the complete documentation bundle
```

## Where to read more

Each question below has one page that owns it; go straight there rather than
reading in order.

- getting and identifying the binary — [awa help install](install.md)
- a fresh project, first loop — [awa help quickstart](quickstart.md)
- structuring a review or repeated fix loop —
  [awa help workflows](workflows.md)
- putting paths back after a rewrite — [awa help restore](restore.md)
- reading the dashboard — [awa help status](status.md)
- finding and reading an earlier run's output —
  [awa help inspect](inspect.md)
- something is slow, stale, blocked, or broken —
  [awa help troubleshooting](troubleshooting.md)
- consuming awa from a script — [awa help json](json.md)

## See also

- [awa help run](run.md)
- [awa help record](record.md)
- [awa help checkpoints](checkpoints.md)
- [awa help restore](restore.md)
- [awa help privacy](privacy.md)
- [awa help exit-codes](exit-codes.md)
