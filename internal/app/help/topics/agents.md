# awa for coding agents

Use awa for three things: retained execution results you can read again,
explicit worktree checkpoints, and reusable deterministic checks. No prior
project knowledge is needed.

## Review the worktree

Start a review with the dashboard:

```text
awa status   # checkpoint, dirty summary, reusable/near runs, next commands
```

Before a meaningful change, checkpoint with a specific message; review
against it:

```text
awa checkpoint -m "<specific-checkpoint-message>"
awa changes <checkpoint-id>..now
awa diff <checkpoint-id>..now [-- <path>]
```

Compare against the id `awa checkpoint` prints. Plain `awa changes` and
`awa diff` use `latest..now`, and `latest` moves whenever any agent or user
checkpoints; `awa log -n 5` recovers an earlier id
([awa help checkpoints](checkpoints.md)).

A clean delta means only "no changes since that baseline". Before declaring
work ready, inspect the full uncommitted worktree, not just the delta.

## Keep execution results

A test, build, or diagnostic run through awa keeps its command, exit status, and
captured output, failures included, so you can read it again instead of
rerunning it. When a fresh result is required, record it; the run prints its id:

```text
awa run --record --stdin null --display tail:200 -- <command>
awa run log --command "<text>"   # earlier executions, newest first
awa run show <run-id>            # metadata only: exit, reuse, byte counts
awa run show <run-id> --tail 500 # captured output; or --stdout, --stderr
awa changes run:<run-id>:before..run:<run-id>:after   # what changed while it ran
```

`--stdin null` is only for a command that reads no input; drop it otherwise.
`run ls` and `run explain` answer "reusable now?", not "what ran?": a
non-reusable or record-only run is still evidence you can read.

`--record` always executes and never becomes a reusable hit. Use it for fresh
results and side effects: builds, generators, formatters, migrations,
deployments. Its before/after observation shows which observed paths changed
while it ran, not who wrote them; without the wrapper awa sees drift but no run
window. A check whose build directory neither affects its result nor is needed
after a replay may instead use an exact [validation policy](validation.md),
which replays the result only.

A copyable paragraph for a project's agent instructions:

```text
Run tests, builds, and diagnostics that need a fresh result with
`awa run --record -- <command>`; awa keeps the command, exit status, and
captured output in this project's local store. Note the run id it prints,
especially after a failure, and use it to refer to that result; find an
earlier run with `awa run log --command <text>`. Instead of rerunning a
command only to see its output, read it with `awa run show <run-id> --tail <n>`
(plain `show` is metadata, with the exit), or filter it with
`awa run show <run-id> --grep <pattern>`: piping the live run can hide the id
and report the filter's exit. A tail is not all captured output, and capture
can be truncated or disabled. Only names awa's baseline or project
config admit reach the command; `NAME=value awa run ...` sets a value
but cannot admit `NAME`. See `awa help inspect` and
`awa help privacy`.
```

A run id is local to this project's `.awa/` store, not a shareable link: name
the project with it. Capture limits and retention apply, so no complete or
indefinite log is promised. Trivial shell commands need no wrapper, and a
justified rerun is fine. Finding, filtering, reading runs:
[awa help inspect](inspect.md).

## Inputs you may need to read later

A run keeps its output, status, and each observed input's hash, not its
bytes: `run:<id>:before` can identify a temporary file but never show its text.
When an experiment's input must stay inspectable, preserve that exact state
before running: make the change, then `awa checkpoint`, or keep a safe fixture.
A checkpoint taken before the change does not hold it. Note the project, run
id, and checkpoint id together. Most commands need none of this.

Excluded, protected, and hash-only paths keep no bytes; a probe kept under
`.awa/` is not observed even when the run executes it. Do not weaken privacy
to keep them. Retained bytes alone do not prove the command read them. Steps:
[awa help workflows](workflows.md).

## Reuse a deterministic check safely

Without `--record`, `awa run` still records the run; it becomes a reusable hit
only if it is clean and left its observed inputs and watched generated-output
roots unchanged:

```text
awa run --stdin null --display tail:200 -- <command>
awa run ls                       # reusable now (current inputs and policy)
awa run ls --near                # and why recent runs are not reusable
awa run explain --stdin null -- <command>   # cache decision, no execution
awa run --refresh --stdin null -- <command> # force a fresh check
awa run show --last --grep "<pattern>"      # convenience: the newest run
```

Piped or file stdin, which agent harnesses usually supply, is unkeyed, so
that run is never reusable (`stdin-not-keyed`); the null device is not.

Warnings here are correctness signals, not noise:

- Do not use cache mode for non-idempotent commands or ones that depend on
  hidden external state; a false hit would skip real work.
- A "skipped inputs" or "not cached" notice means the result was not made
  reusable.
- The execution directory is part of the cache key; `--cwd <path>` makes it
  explicit.
- `.gitignore` is OFF by default for awa's scan input, so a gitignored file is
  still a real input that can affect a command.
- A note that a large watched effect root (e.g. `target/`) dominated state
  observation explains a slow run, `run ls --near`, or status; do not rerun
  blindly, and changing `[run]` effect roots is a policy decision.

## Repairing an accidental rewrite

`awa restore <checkpoint-id> -- <path>` previews putting selected paths back
from a checkpoint and changes nothing; only `--apply` mutates, and it needs
explicit paths or `--all`. Incomplete evidence refuses with a named reason;
there is no force option. The `restore:<id>:before` undo reference is kept
under `[gc].keep_restores_for`: evidence with a lifetime, not a backup. See
[awa help restore](restore.md).

## Local evidence is private, not shared

- `.awa/` holds durable evidence (command lines, captured output, cwd,
  manifests, content blobs). Do not commit it; awa keeps it owner-private and
  untracked, restoring its `.gitignore` guard on checkpoint/run.
- Allowlisted env values are keyed and passed to the child, but stored only as
  a redacted identity, never raw. Even so, do not print secrets through a
  wrapped command: captured stdout/stderr is stored verbatim as evidence.
- An executed child gets `AWA_RUN=1`: advisory and forgeable, it grants and
  proves nothing.
- After a repo is moved, copied, or shared, `awa doctor` checks the guard,
  permissions, and root hygiene.

## Discovering awa and machine output

`awa version`, `awa help topics`, and `awa docs export --output <directory>`
(the complete documentation bundle) answer for the installed version, offline.
For automation, use `--json` instead of parsing human text (e.g.
`awa status --json`); see [awa help json](json.md).

## Where to read more

Each question has one owning page:

- getting and identifying the binary — [awa help install](install.md)
- a fresh project, first loop — [awa help quickstart](quickstart.md)
- a review, fix loop, or repeated attempts — [awa help workflows](workflows.md)
- reading the dashboard — [awa help status](status.md)
- finding, filtering, and reading a run — [awa help inspect](inspect.md)
- something slow, stale, blocked, or broken —
  [awa help troubleshooting](troubleshooting.md)
- reviewing an existing project's setup — [awa help maintenance](maintenance.md)

See also [awa help run](run.md), [awa help record](record.md),
[awa help privacy](privacy.md), and [awa help exit-codes](exit-codes.md).
