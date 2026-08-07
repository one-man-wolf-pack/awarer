# awa checkpoint — explicit checkpoints

```text
awa checkpoint [-m <message>]
```

`awa checkpoint` records an explicit checkpoint of the whole configured worktree
scope. These are user-authored markers of "what did this project look like at
this moment".

```text
awa checkpoint -m "<specific-checkpoint-message>"
```

`<specific-checkpoint-message>` is a short description of this baseline; replace
the placeholder, and do not copy it literally.

A checkpoint has one durable address: the immutable id `awa checkpoint` prints.
The message explains the checkpoint to a human and is never used to look one up.
Use a specific message each time — do not reuse one generic message forever.

`awa checkpoint` also prints copyable follow-up ranges
(`awa changes <checkpoint-id>..now`, `awa diff <checkpoint-id>..now`); keep the
id if a review/fix loop will continue, because `latest` is shared project-local
state and can move if another agent checkpoints. Any unambiguous leading portion
of an id works wherever the full id does (see [awa help refs](refs.md)).

`awa checkpoint` takes no path argument — it always covers the full scope; a
path is a usage error. Path filters belong to comparison output (see
[awa help diff](diff.md)), not to partial checkpoints.

What a checkpoint records is project policy, not a per-invocation flag, and it
lives in the `[checkpoint]` config table: whether file contents are stored or
only hashed, whether renames are detected, and the default diff context. Which
paths are in scope is a separate decision with its own rules (see
[awa help ignores](ignores.md)); within that scope a checkpoint always includes
files git does not track, and no setting changes that. Turning off
`store_file_contents` is the one that changes a checkpoint's recorded storage
policy, so records made before and after that change are not equivalent
evidence. The keys and their defaults are in
[awa help config](../reference/configuration.md).

## Listing

```text
awa log        explicit checkpoints only (the default timeline)
awa log --all  explicit checkpoints plus recorded runs
```

Comparisons default to the range `latest..now`, so right after a checkpoint:

```text
awa changes    # summarize changes since the latest checkpoint
awa diff       # textual diff, latest..now
```

A clean delta only means "no changes since that checkpoint" — do a full
uncommitted-worktree pass before final acceptance, not just the checkpoint
delta.

## A checkpoint is also a repair source

Comparison is not all a checkpoint is for: `awa restore` uses one as the
evidence for putting selected paths back after an accidental generator or
formatter rewrite. Preview first, name the paths, and let `--apply` be the
deliberate step — see [awa help restore](restore.md).

## See also

- [awa help diff](diff.md)
- [awa help restore](restore.md)
- [awa help refs](refs.md)
- [awa help workflows](workflows.md)
- [awa help status](status.md)
