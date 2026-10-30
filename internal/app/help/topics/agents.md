# awa for coding agents

Use awa for three things: retained execution results you can read again,
explicit checkpoints of the worktree, and reusable deterministic checks. The
commands below are the whole workflow; no prior knowledge of the project is
needed.

## Review the worktree

Start a review with the dashboard:

```text
awa status                       # checkpoint, dirty summary, reusable/near
                                 # runs, and the commands to run next
```

Before a meaningful change, checkpoint with a specific message, then review
what changed since it:

```text
awa checkpoint -m "<specific-checkpoint-message>"
awa changes <checkpoint-id>..now
awa diff <checkpoint-id>..now [-- <path>]
```

Compare against the id `awa checkpoint` prints. Plain `awa changes` and
`awa diff` use `latest..now`, and `latest` moves whenever any agent or user
checkpoints; `awa log -n 5` shows recent checkpoints.

A clean delta means only "no changes since that baseline". Before you declare
work ready, inspect the full current uncommitted worktree, not just the delta.

## Keep execution results

A test, build, or diagnostic run through awa keeps its command, exit status, and
captured output, failures included, so you can read it again instead of
rerunning it. When a fresh result is required, record it; the run prints its id:

```text
awa run --record --stdin null --display tail:200 -- <command>
awa run log --command "<text>"   # earlier executions, newest first
awa run show <run-id>            # metadata only: exit, reuse, byte counts
awa run show <run-id> --tail 500 # captured output; or --stdout, --stderr
awa changes run:<run-id>:before..run:<run-id>:after   # what it changed
```

`--stdin null` is only for a command that reads no input; drop it otherwise.
`run ls` and `run explain` answer "reusable now?", not "what ran?": a
non-reusable or record-only run is still evidence you can read.

`--record` always executes and never becomes a reusable hit. Use it for fresh
results and side-effecting work: builds, generators, formatters, migrations,
deployments. Its before/after observation shows which observed paths the
command changed; without the wrapper awa sees drift but not its cause. A check
whose build directory neither affects its result nor is needed after a replay
may instead use an exact [validation policy](validation.md), which replays the
result only.

A copyable paragraph for a project's agent instructions:

```text
Run tests, builds, and diagnostics that need a fresh result with
`awa run --record -- <command>`; awa keeps the command, exit status, and
captured output in this project's local store. Note the run id it prints,
especially after a failure, and use it to refer to that result; find an
earlier run with `awa run log --command <text>`. Before rerunning a command
only to see its output again, read it with `awa run show <run-id> --tail <n>`
(plain `show` prints metadata only). A tail is not all captured output, and
capture can be truncated or disabled. See `awa help inspect`.
```

A run id is local to this project's `.awa/` store, not a shareable link: name
the project with it. Capture limits and retention apply, so no complete or
indefinite log is promised. Trivial shell commands need no wrapper, and a
justified rerun is fine. Finding and reading runs: [awa help inspect](inspect.md).

## Inputs you may need to read later

A run keeps its output, its status, and each observed input's hash, not its
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
awa run ls --near                # and why recent checks are now stale
awa run explain --stdin null -- <command>   # cache decision, no execution
awa run --refresh --stdin null -- <command> # force a fresh check
awa run show --last --grep "<pattern>"      # convenience: the newest run
```

A pipe or file on stdin, which agent harnesses usually supply, is passed
through unkeyed, so that run is never reusable (`stdin-not-keyed`); the null
device is not.

Warnings here are correctness signals, not noise:

- Do not use cache mode for non-idempotent commands or commands that depend on
  hidden external state; a false hit would skip real work.
- A "skipped inputs" or "not cached" notice means the result was not made
  reusable.
- The execution directory is part of the cache key; `--cwd <path>` makes it
  explicit.
- `.gitignore` is OFF by default for awa's scan input, so a gitignored file is
  still a real input that can affect a command.
- A note that a large watched effect root (e.g. `target/`) dominated state
  observation explains a slow run, `run ls --near`, or status. Do not rerun
  blindly; changing the `[run]` effect roots is a policy decision.

## Repairing an accidental rewrite

`awa restore <checkpoint-id> -- <path>` previews putting selected paths back
from a checkpoint and changes nothing; `--apply` is the only mutating form and
needs an explicit selection (paths or `--all`). Incomplete evidence refuses with
a named reason; there is no force option. The `restore:<id>:before` undo
reference is retained under `[gc].keep_restores_for`: evidence with a lifetime,
not a backup. See [awa help restore](restore.md).

## Machine output

For automation, add `--json` instead of parsing human text, e.g.
`awa status --json` or `awa run ls --near --json`; see
[awa help json](json.md).

## Local evidence is private, not shared

- `.awa/` holds durable evidence (command lines, captured output, cwd,
  manifests, content blobs). Do not commit it — awa keeps it owner-private and
  untracked and restores its `.gitignore` guard on checkpoint/run.
- Allowlisted env values are keyed and passed to the child, but stored only as
  a redacted identity, never raw. Even so, do not print secrets through a
  wrapped command: captured stdout/stderr is stored verbatim as evidence.
- An executed child gets `AWA_RUN=1`. It is advisory and forgeable: it grants
  nothing and proves nothing.
- After a repo is moved, copied, or shared, `awa doctor` checks the guard,
  permissions, and root hygiene.

## Discovering awa itself

`awa version`, `awa help topics`, and `awa docs export --output <directory>`
(the complete documentation bundle) answer for the installed version, offline.

## Where to read more

Each question below has one owning page:

- getting and identifying the binary — [awa help install](install.md)
- a fresh project, first loop — [awa help quickstart](quickstart.md)
- a review or repeated fix loop — [awa help workflows](workflows.md)
- reading the dashboard — [awa help status](status.md)
- finding and reading an earlier run — [awa help inspect](inspect.md)
- something slow, stale, blocked, or broken —
  [awa help troubleshooting](troubleshooting.md)
- reviewing an existing project's setup — [awa help maintenance](maintenance.md)

See also [awa help run](run.md), [awa help record](record.md),
[awa help checkpoints](checkpoints.md), [awa help privacy](privacy.md), and
[awa help exit-codes](exit-codes.md).
