# awa restore — repair selected paths from an immutable state

```text
awa restore <state-ref> -- <path>            # preview (default)
awa restore --dry-run <state-ref> -- <path>  # the same preview, spelled out
awa restore --apply <state-ref> -- <path>    # the only mutating form
awa restore --apply --all <state-ref>
```

`awa restore` repairs the worktree from one immutable stored state. It exists for
mistakes made while the worktree is dirty: a generator that rewrote a subtree, a
formatter applied to the wrong directory, a handful of files that should go back
to a reviewed checkpoint.

It is not a Git replacement. It does not read commit history, does not check out
a commit, and does not touch files outside awa's evidence.

## Preview first, apply on purpose

Preview is the default and changes nothing: no worktree path, and no stored awa
state. `--dry-run` selects that same mode explicitly; combining it with `--apply`
is a usage error, because the two name different modes.

A selection is mandatory: one or more paths, or an explicit `--all`. An omitted
selection is a usage error, never a whole-project restore. `--all` means all
**proven restorable awa scope**, not every file under the project root.

There is deliberately no force option. When the evidence is incomplete, restore
refuses and tells you which reason blocked which path.

```text
awa restore <checkpoint-id> -- generated/client
awa restore --apply <checkpoint-id> -- generated/client
awa changes <checkpoint-id>..now -- generated/client
```

The preview prints the exact apply command using the **resolved immutable id**,
never the moving reference you typed, so running the suggestion cannot act on a
state that moved in between.

## Sources

The source must resolve to immutable stored evidence:

- a checkpoint id or an unambiguous id prefix;
- `latest` or `@-N`, resolved once and reported as a full checkpoint id;
- `run:<id>:before` or `run:<id>:after`, while that observation is retained. A run
  observation records identity only: it is useful for comparison, for absence
  inside its observed scope, and for a symlink's stored target, but it supplies no
  regular-file bytes, so restoring one is reported as `hash-only-content`;
- `restore:<id>:before`, a previous restore's recovery observation, while it is
  retained.

`now` is not a source, and a range is not accepted: the source supplies the
desired bytes, and the current worktree is always the destination. See
[awa help refs](refs.md) for the reference syntax.

## What it can and cannot prove

An entry hash proves identity, not recoverability. A regular file is restorable
only when its source state points at a verified content blob. `hash-only`
evidence stays useful for comparison but cannot manufacture bytes, and awa never
recovers content from a current file that happens to match, from another path,
from Git, or from the network.

Symlinks are recreated from their stored target and are never followed. Special
files (devices, sockets, fifos) are not restored.

A path *reached through* a symlink — which only exists when the project enables
`follow_symlinks` — is a boundary rather than work. awa writes and deletes through
a no-follow descent, so it cannot act at a path whose components belong to a link's
target: a change there is reported as `symlink-ancestor` instead of previewing as
work that would fail. An unchanged path reached that way needs no mutation and is
simply counted as already-matching scope.

Deletion needs positive proof: the path must be currently observed and provably
absent from a **policy-compatible** source. If the source was observed under a
different scan policy, absence is not proof — the path may simply have been out
of scope then — so deletions are not planned. Directory removal is empty-only and
deepest-first; awa never recursively deletes a worktree path.

Ignored paths are outside awa's evidence boundary. They are never restored and
never deleted for being absent from a source manifest.

## Blocked reasons

When a path cannot be restored, the preview names why with a stable token:
`hash-only-content`, `blob-missing`, `blob-corrupt`, `skipped-boundary`,
`ignored-boundary`, `policy-incompatible`, `observation-unstable`,
`out-of-proven-scope`, `path-conflict`, `root-escape`, `symlink-ancestor`, or
`unsupported-entry-kind`. `--apply` refuses while any of them applies.

Selecting a path that awa cannot see is reported as `ignored-boundary` rather
than as an empty result: "nothing to do" and "this path is outside my evidence"
are different answers, and only one of them means the path is already correct.
Selecting a path a `restore:<id>:before` source never covered is reported the
same way, as `out-of-proven-scope` — that record proves exactly the paths one
restore was going to change and holds no evidence about anything else.

## Undo: the recovery observation

Before its first write, `--apply` durably records the exact current state it may
replace or delete — with verified content — as an immutable **recovery
observation**, and prints its `restore:<id>:before` reference:

```text
recovery: restore:<operation-id>:before
undo:    awa restore --apply restore:<operation-id>:before -- generated/client
```

Content is captured whatever the project's storage preferences are — including
`[checkpoint] store_file_contents = false` and a file large enough that
`[hashing] large_file_policy = hash-only` recorded its identity only. Those bytes
are exactly what would otherwise be unrecoverable, and a readable file stays
readable no matter what a checkpoint would have stored. If the capture cannot be
completed, apply refuses before mutating.

It preserves what the worktree held when it was recorded, and nothing else. Bytes
written after that moment are not in it — which is why apply re-proves each
selected file's content identity immediately before touching it and reports a
conflict instead of overwriting work no evidence describes.

A recovery observation is system-owned evidence, not a checkpoint. It never
appears in `awa log`, never moves `latest`, and is never selected by a
checkpoint-relative reference; it shows up in `awa log --all` and as a state
reference. It is one immutable record per applied restore, not a permanent backup
and not an undo stack.

Its retention is `[gc].keep_restores_for` (default `14d`), independent of
`keep_runs_for`. Ordinary `awa gc` reclaims older observations, and an explicit
`awa gc --older-than` overrides the window for that invocation — so treat it as
evidence with a lifetime, not as a backup.

## When a multi-path apply stops

Filesystem mutation across several paths is not transactional. If an external
writer, an I/O failure, or an interruption stops a commit after the first path,
restore reports `partial` with completed and remaining counts — never success.
Rerunning re-plans from what the worktree is now and does only the remaining
work.

A file replacement leaves its destination holding either the old content or the
complete new content: the bytes are written to a temporary file in the same
directory and renamed into place. Two shapes cannot be done in one step —
replacing a directory with a file or a link, and replacing a file or a link with
a directory — because the old node has to be removed first. If a commit stops
between those two steps that one path is left absent, and restore still reports
`partial` even when no operation completed. It never reports `conflict` or
`cancelled` in that case: those say the worktree was not touched. The recovery
observation is what the absent path is restored from.

Restore acquires the locks that serialize it against another restore and keep
`awa gc` from reclaiming the blobs it is reading. It cannot lock your editor or
your build tool; it observes the selected state twice and, immediately before
touching each path, re-proves that path's kind, permission bits, symlink target,
and — for a regular file — its content identity. Only a write that lands between
that final read and the mutation itself can escape, which is the honest boundary.

## Machine output

`--json` emits one typed document carrying the `awa-restore/v1` contract token,
the full resolved source identity, the normalized selection, deterministic
counts, the evidence boundary, stable reason tokens, completed/remaining facts,
and the recovery observation's id and reference. It never contains file bytes.
See [awa help json](json.md).

## See also

- [awa help checkpoints](checkpoints.md) — recording the states restore reads
- [awa help refs](refs.md) — the reference syntax
- [awa help diff](diff.md) — inspecting a delta before and after a restore
- [awa help gc](gc.md) — retention of recovery observations
