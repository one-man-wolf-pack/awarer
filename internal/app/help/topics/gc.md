# awa gc — reclaim unreferenced local state

```text
awa gc [--dry-run] [--committed] [filters]
```

`awa gc` removes state no longer reachable from a retained checkpoint, run, or
restore recovery observation — unreachable blobs, expired runs, run entries in a
schema this awa cannot read, expired checkpoints, expired recovery observations,
and stale temp artifacts — under the retention policy in the
`[gc]` config section. It takes the exclusive collector lease and waits
(bounded by `[locks].timeout`) only for another collector holding it. It never
waits out a writer: an in-flight checkpoint or run suppresses deletion outright
rather than being raced — see Safety below.

```text
awa gc --dry-run        # show what would be removed and what is retained
awa gc                  # reclaim now
awa gc --json           # machine-readable plan + execution summary
```

Both a dry run and a real run report the deletion candidates AND the retained or
blocked ones, so the decision is always visible: you can see WHY nothing was
removed, not just how much was kept.

## Why gc decided what it decided

Every candidate carries one token from a single closed vocabulary, and each token
maps to exactly one action — there is no "delete because it is the latest". This
is that vocabulary, grouped by the action it justifies. The tokens are stable
machine values: switch on them rather than on the surrounding prose.

Deleted — proved disposable:

```text
checkpoint-expired          a checkpoint past the retention window
run-expired                 a run older than the run retention window
blob-unreachable            a blob no retained checkpoint, run, or recovery
                            observation references
temp-stale                  a temp artifact left behind by a finished operation
restore-expired             a restore recovery observation past keep_restores_for
```

Retained — a retention rule protects it:

```text
checkpoint-latest           the newest checkpoint, always kept
checkpoint-within-keep-last inside the keep_last_checkpoints window
checkpoint-too-recent       newer than the --older-than cutoff
checkpoint-not-committed    not covered by the current HEAD commit (--committed)
run-too-recent              newer than the --older-than cutoff
run-not-committed           not covered by the current HEAD commit (--committed)
blob-referenced             still referenced by a retained checkpoint, run, or
                            recovery observation
temp-fresh                  a temp artifact of a possibly in-flight operation
restore-too-recent          a recovery observation inside keep_restores_for
git-unavailable             git metadata could not be read, so nothing is assumed
```

Blocked — an obstruction stands in the way and nothing is removed:

```text
lock-active                 another writer holds the lock
lock-unknown                a lock file awa cannot classify
checkpoint-corrupt          a checkpoint record that cannot be planned safely
checkpoint-incompatible     a checkpoint record in a schema this awa cannot read
run-corrupt                 a run record that cannot be planned safely
run-incompatible            a run record in a schema this awa cannot read
restore-corrupt             a recovery observation that cannot be read, so blob
                            reachability is incomplete and the sweep stands down
```

Skipped — excluded by a subsystem filter:

```text
subsystem-filtered          a restriction flag excluded an otherwise-deletable item
```

The two blocked families differ in exit status; see Safety below.

## Restore recovery observations

An applied `awa restore` records the pre-restore state it overwrote as an
immutable recovery observation (see [awa help restore](restore.md)). It is the
only evidence that restore can be undone, so it has its own retention window,
`[gc].keep_restores_for`, defaulting to `14d` and independent of
`keep_runs_for` — shortening run history must never silently shorten the undo
window.

Recovery observations participate in ordinary unfiltered `awa gc`. `--runs-only`,
`--checkpoints-only`, and `--blobs-only` exclude them, and there is no
`--restores-only` flag. An explicit `awa gc --older-than <dur>` overrides the
configured window for that invocation, so awa does not print an "available until"
date it cannot guarantee.

`--committed` does not classify them: a commit advancing does not make the
pre-restore state of an uncommitted worktree disposable, so only the age rule
applies. An observation protected by an in-flight restore is retained, and one
that cannot be read blocks the blob sweep rather than being guessed at.

## Narrowing what a run considers

```text
--committed             also reclaim WIP evidence from before the current commit
--older-than <dur>      only consider state older than a duration (e.g. 30d)
```

Per-subsystem restrictions and the retention override are listed in the
generated [gc command reference](../commands/gc.md); this page covers when to
reach for them rather than restating the flag table.

Use `--committed` after a commit to reclaim the previous WIP cycle's evidence.
It is conservative: it uses the current HEAD commit time only as an eligibility
cutoff (never as proof of review), retains the latest checkpoint, and deletes
nothing when git metadata is unavailable. Inspect it first with
`awa gc --committed --dry-run`.

The latest checkpoint is always retained, and so is every checkpoint inside the
`keep_last_checkpoints` window. To keep more history, widen that window rather
than protecting individual checkpoints — retention is a policy, not a per-record
mark.

## Safety

The destructive pass is all-or-nothing. If any candidate is blocked — by an active
or unknown writer lock, by storage corruption, or by a record in a schema this awa
cannot read — `gc` deletes nothing at all, not merely the blocked item. It never
sweeps around an obstruction: a live writer may be publishing state the plan was
computed against, so a partial sweep could reclaim something that just became
reachable.

```text
a writer holds a lock   whole plan suppressed, nothing deleted   exit 0
state needs a decision  whole plan suppressed, nothing deleted   exit 5
another gc is running   never gets to plan; waits out the lease  exit 6
```

The obstruction is always reported, so you can see what stood in the way. The
first two cases differ only in meaning: a lock is a normal "try again later",
while corruption or an unreadable schema needs a person — run `awa doctor` first
when `gc` reports either, and the report names which one it is. The third is a
different mechanism: a second `gc` competing for the exclusive collector lease
waits `[locks].timeout` and then fails with the lock-timeout code, so a script
that treats every busy lock as exit 0 misreads it.

What a blocked run does NOT give you is a complete picture of what could be
reclaimed. `gc` reports the candidates it classified safely and skips the stages it
cannot classify safely, with a warning naming the skip. In particular the blob
sweep needs a complete reachability set, so a lock, a checkpoint or run record it
cannot read, or an unanchored `--committed` cutoff makes it report only the blobs it
proved *referenced* and classify no unreachable ones at all — a false
"unreachable" would be data loss. The store's total footprint is still measured
from the complete listing, but the would-free figure beside it is missing whatever
that skipped stage would have found. Treat a blocked plan as "here is the
obstruction", not as an estimate; clear the blocker and re-run
`awa gc --dry-run` for an authoritative reclaim set.

Freed/would-free bytes are the accounted bytes, summed from readable entries. An
entry whose metadata does not decode contributes 0, because its size is never
read — the reported figure is known/accounted bytes, not a guarantee of the exact
physical space freed.

`gc` never removes a record it cannot read. A record in a schema this awa does
not speak is intact evidence with an unknown reference model: nothing can prove
what it points at, so deleting it could orphan or destroy referenced content. It
is retained under `checkpoint-incompatible` or `run-incompatible`, the blob sweep
stands down, and `awa gc` exits with the state-action-required code. Resolving it
is a deliberate act: drop a single stored run with `awa run rm <id>`, or reset the
store by deleting the evidence directories under `.awa/` and running `awa init`
(see [awa help install](install.md)). Delete those directories rather than `.awa/`
itself, which would take your private `.awa/config.toml` with it.

## See also

- [awa help doctor](doctor.md)
- [awa help checkpoints](checkpoints.md)
- [awa help inspect](inspect.md)
- [awa help troubleshooting](troubleshooting.md)
