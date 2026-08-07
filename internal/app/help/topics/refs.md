# state reference syntax

Commands that name a stored state — `changes`, `diff`, and `restore` — share one
reference syntax:

```text
now                     current workspace state
latest                  latest checkpoint
@-1                     latest checkpoint
@-2                     previous checkpoint
@-N                     Nth checkpoint from the end (N >= 1)
<checkpoint-id>         a checkpoint by its full immutable id
<checkpoint-id-prefix>  a checkpoint by any unambiguous leading portion of it
run:<id>:before         a stored run's observed state before it ran
run:<id>:after          a stored run's observed state after it ran
restore:<id>:before     an applied restore's pre-restore recovery observation
```

Ranges use `A..B`:

```text
latest..now
@-2..@-1
<checkpoint-id>..now
run:<id>:before..run:<id>:after
```

If a range is omitted, commands use `latest..now`. For `changes` and `diff`, the
numeric shortcut `-N` means `@-N..now`.

A checkpoint's only durable address is its immutable id. A checkpoint message is
human context and is never a reference. A token that cannot be an id or id prefix
is rejected as invalid syntax before any stored state is read; a well-formed
prefix that matches nothing is not found, and one that matches several
checkpoints fails as ambiguous rather than picking one — lengthen it.

`latest` is shared project-local state, not an agent-private bookmark: it always
names the newest checkpoint, so it moves whenever any agent or user runs
`awa checkpoint`. For precision across a long review/fix loop, keep the id
`awa checkpoint` printed and use an explicit range (`<checkpoint-id>..now`)
instead of relying on `latest`. Use `awa log -n 5` to inspect recent checkpoints
when a default baseline is surprising.

The `run:` forms work for any stored run, not only a `--record` one: an ordinary
cached run and a failed run are addressable the same way. A run observation stores
identity, not content: it compares, it proves what was absent inside its observed
scope, and it keeps a symlink's target, but it holds no regular-file bytes — so
`awa diff` against one degrades to hash-only and `awa restore` from one reports
`hash-only-content` rather than writing a file.

The `restore:` form names the immutable state an applied `awa restore` recorded
before its first write. Unlike a run observation it carries content for every
regular file it covers — that is the point of it — whatever storage policy the
project applies to checkpoints. There is no `restore:<id>:after`: the state after a
restore is the current worktree, which `now` already names. It stays resolvable
while `[gc].keep_restores_for` retains it — see
[awa help restore](restore.md). It is system-owned evidence, so it never appears
as a checkpoint and is never selected by `latest` or `@-N`; `awa log --all` lists
it and `awa state resolve` deliberately does not name it.

`awa run explain` does not use this syntax: it selects a run with `--last` or
`--from-run <id> --to-now`, not a state reference or `A..B` range.

Git commit boundaries shown in `awa log --all` are context markers, not awa
records: they carry no reference token and cannot be used as a state reference
or in an `A..B` range.

## See also

- [awa help diff](diff.md)
- [awa help restore](restore.md)
- [awa help record](record.md)
- [awa help checkpoints](checkpoints.md)
- [awa help workflows](workflows.md)
