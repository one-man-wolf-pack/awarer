# coding, review, specification, and fix loops

This page is about structure: which awa commands to run, in what order, so that a
multi-step task keeps an honest record of what changed and what was verified.
The individual commands are documented on their own pages; what follows is how
they compose.

## Pick a baseline and keep it

Every comparison needs a baseline. The default one is `latest`, the newest
checkpoint — and `latest` is shared project-local state, not your private
bookmark. It moves whenever any agent or user runs `awa checkpoint`, including
in another terminal.

For anything longer than a single edit, capture the id instead:

```text
awa checkpoint -m "<specific-checkpoint-message>"
awa changes <checkpoint-id>..now
awa diff <checkpoint-id>..now
```

`<specific-checkpoint-message>` is the short description of this baseline;
`<checkpoint-id>` is the id `awa checkpoint` printed (a unique prefix is enough).
Using the explicit range means another agent's checkpoint cannot silently move
what you are comparing against.

If a default baseline ever surprises you:

```text
awa log -n 5
```

## Coding loop

```text
awa checkpoint -m "<specific-checkpoint-message>"
awa changes
awa run -- <command>
```

Checkpoint before a meaningful change, make the change, review the delta, then
run the check you care about. `<command>` is your test, build, or lint
invocation. A second `awa run` of the same command replays the stored result
instead of re-executing — when the inputs, working directory, and keyed
environment are unchanged, and the first run was clean and non-mutating, which
is what made it publishable at all. Anything else is a miss, and a miss is
information rather than a fault: see [awa help run](run.md).

## Review loop: two passes

A review is finished only after both passes.

First pass — focused review memory. What changed since the baseline you chose:

```text
awa changes <checkpoint-id>..now
awa diff <checkpoint-id>..now
```

Second pass — the full current uncommitted surface, using the project's normal
review tooling (`git status`, `git diff`, or whatever this repository uses).

An empty first pass means only "nothing changed since that baseline". It is
never proof that the whole worktree has been reviewed, and a checkpoint delta is
not the same set of changes as the uncommitted git diff. In a git repository,
`awa changes` and `awa diff` name the current HEAD and add a note when the
baseline predates or diverges from it — after commits, rebases, or amends. That
note is context, not a fault: the comparison is still valid, but the delta may
not map onto current HEAD, which is exactly when the second pass matters most.

## Repeated fix loop

When a reviewer asks for several fixes in sequence, make each round
individually verifiable:

```text
awa checkpoint -m "<specific-checkpoint-message>"
awa run -- <command>
awa changes <checkpoint-id>..now
```

One checkpoint per fix request, with a message naming that request. Re-run the
same check after each fix. When a check that passed a moment ago no longer
reuses its earlier result, ask why rather than assuming breakage:

```text
awa run ls --near
awa run explain -- <command>
```

Do not reach for `--refresh` or `--no-cache` to make a loop feel faster: a miss
is a statement about the current inputs, and an old successful run is not
reusable for state that changed. See
[awa help troubleshooting](troubleshooting.md).

## Specification and other long-running work

For work spanning many sessions, keep the id each anchor checkpoint prints. An
id never moves, so it stays a precise baseline no matter how many checkpoints are
created after it — unlike `latest`, which always names the newest one:

```text
awa checkpoint -m "<specific-checkpoint-message>"
awa changes <checkpoint-id>..now
```

Record the id wherever the work itself is tracked, alongside the message that
says what the anchor is — for example the identifier of the specification section
you are implementing.

To see the whole timeline rather than only checkpoints:

```text
awa log --all
```

That adds recorded runs and git commit boundaries as context markers. The commit
boundaries are markers only; they are not awa state references.

After committing a cycle, start a fresh checkpoint for the next one and inspect
what local evidence became redundant:

```text
awa gc --committed --dry-run
```

## Steps with side effects

A command that deploys, migrates, formats, or otherwise changes the worktree must
not be replayed from a cache. Record it instead, then inspect what it did:

```text
awa run --record -- <command>
awa changes run:<id>:before..run:<id>:after
```

`<id>` is the run id the record printed. A recorded run always executes and never
becomes a reusable hit — that is the point of it.

## Several agents in one worktree

`.awa/` is shared project-local state, so concurrent agents see each other's
checkpoints and runs. Two habits keep that safe: always compare against an
explicit checkpoint id rather than `latest`, and give each checkpoint a message
specific enough that another agent can tell whose baseline it is. When a lock is
briefly held by another process, awa reports it rather than waiting forever; see
[awa help exit-codes](exit-codes.md).

## See also

- [awa help agents](agents.md)
- [awa help status](status.md)
- [awa help checkpoints](checkpoints.md)
- [awa help diff](diff.md)
- [awa help refs](refs.md)
- [awa help run](run.md)
- [awa help record](record.md)
