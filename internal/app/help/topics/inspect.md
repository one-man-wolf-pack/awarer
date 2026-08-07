# finding stored runs and their output

Every command executed through `awa run` is recorded, and its output is stored
durably whether the run was cacheable or not — as long as capture is on, which it
is by default. A project that sets `[run].capture_output = false` still gets the
run in the history, but with nothing to read (reuse reason `capture-disabled`).
This page is how you find a run again and read what it printed — instead of
re-running it, or redirecting to a scratch file you will have to remember the name
of.

## Find the run

```text
awa run log
awa run log -n 10
awa run log --command <substring>
awa run ls
awa run ls --all
```

`<substring>` matches anywhere in the recorded command line.

`awa run log` is chronological history, newest first: everything that was
recorded, including failed, mutating, and record-only runs. `awa run ls` is a
different question — which stored runs are reusable for the *current* state — so
a run missing from `ls` is not missing from history. `awa run ls --all` lists the
non-reusable ones too, each with its reason token.

There is no time-range filter. To find a run from a particular moment, narrow
with `--command` and read the timestamps, choosing how they are rendered with
`--time relative`, `--time local`, or `--time utc`.

`awa run log` takes its default rendering from the `[ui]` config table, so a
project that prefers absolute timestamps sets it once instead of passing `--time`
every call. `awa run show` is the exception: it reads stored records only and
loads no configuration, so its default stays `relative` whatever `[ui]` says.
`--json` is unaffected either way — machine output is always UTC.

## Read the output

```text
awa run show --last
awa run show --last --tail 500
awa run show --last --grep "<pattern>"
awa run show <id> --meta
awa run show <id> --stdout
awa run show <id> --stderr
```

`<id>` is a run id (a unique prefix is enough) and `<pattern>` is a regular
expression. Exactly one of an id or `--last` selects the run.

- `--meta` is metadata only: command, cwd, exit, duration, cacheability, reuse
  state, mutation outcome, key, and per-stream byte counts.
- `--stdout` and `--stderr` write one raw stream and nothing else, so they are
  safe to pipe.
- `--tail <n>` and `--grep <pattern>` filter what is shown; combined with a
  stream selector they filter that stream, otherwise both streams are shown under
  labelled headers. A tailed view says `(tail — earlier lines omitted)` so a
  partial view is never mistaken for the whole log.

`awa run show` reads stored records only. It never scans the current worktree and
never loads project configuration, so it keeps working when the tree has moved on
or a config file is malformed.

## What the evidence does and does not prove

Stored output is evidence of what that execution printed at that time. It is not
a statement about the current state, and reading it is not the same as re-running
the command.

`awa run show --json` is metadata by default and reports each stream's integrity
as `unverified`: the record was inspected, but no payload bytes were opened.
Integrity becomes `verified` for a stream only when an explicit `--tail` or
`--grep` read opens it and checks it against its recorded hash — and a stream
that fails that check is a loud error, never a document quietly claiming
`verified`. A stream you did not select stays `unverified`.

Output beyond the capture limit is stored truncated, and every surface says so
with the omitted byte count (`(stored <n>, <n> omitted)`). A partial payload is
never presented as complete.

Captured stdout and stderr are stored verbatim and are never redacted, so a
command that printed a secret stored that secret. See
[awa help privacy](privacy.md).

## What a run changed

Every stored run — a record-only one, an ordinary cached one, a failed one — has
an observed state before and after it, usable as state references:

```text
awa changes run:<id>:before..run:<id>:after
awa diff run:<id>:before..run:<id>:after
```

That is the honest way to answer "what did that command actually do", and it is
the standard follow-up after a record-only run. Where one side has no stored file
contents, `diff` reports the change as hash-only rather than inventing content.

## Delete stored runs

```text
awa run rm <id>
awa run rm --command <substring> --dry-run
awa run rm --older-than <duration>
```

`<duration>` accepts `d`, `h`, `m`, and `s` units, so `30d` and `720h` are both
valid. Explicit ids and the filter flags are mutually exclusive, and `--dry-run`
reports what would be removed without removing it.

Deleting a run deletes the evidence, not the effect: whatever the command did to
the worktree stays done. For routine cleanup driven by policy rather than by hand,
prefer [awa help gc](gc.md).

## The wider timeline

```text
awa log
awa log --all
```

`awa log` lists explicit checkpoints only. `awa log --all` interleaves recorded
runs and git commit boundaries as context markers — the commit boundaries are
markers, not awa state references.

## See also

- [awa help run](run.md)
- [awa help record](record.md)
- [awa help refs](refs.md)
- [awa help privacy](privacy.md)
- [awa help gc](gc.md)
- [awa help json](json.md)
