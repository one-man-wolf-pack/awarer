# awa status — the review dashboard

```text
awa status
awa status --json
```

`awa status` is the single entry point before a review, and the default command
when `awa` is run with no arguments. It answers "where am I, what has drifted,
what is reusable, what should I do next" in one bounded pass — prefer it over a
sequence of `awa changes`, `awa run ls`, and `git status`.

It never executes anything and never modifies durable records.

## Durable facts

The first block describes the project and its store:

```text
root:        <project-root>
config:      built-in defaults (no awa.toml or .awa/config.toml)
initialized: yes
checkpoints: 3
  latest:    <short-id> at <timestamp> (0 skipped)
run cache:   7 runs
  latest:    <short-id> exit=0 at <timestamp>
store:       <store-path>
```

The `config:` line lists the layer files that actually exist, or says that the
built-in defaults are in force. `skipped` counts inputs the scan could not read
for that checkpoint. Sub-lines appear only when they have something to report:
`unreadable:` for checkpoints that cannot be decoded (naming how many are in a
schema this awa cannot read, when any are), and `corrupt:` or `incompatible:` for
stored runs, each qualified by the newest-N sample they were counted in.

## The review dashboard

The second block is the review state itself:

```text
checkpoint:  checkpoint <short-id> "<message>" (<age>)
  dirty:     4 changed (1A 2M 1D 0R 0T 0S) — awa changes --stat
git:         branch <name> @ <short-commit> (dirty)
reusable:    2 run(s) replayable now — awa run ls
next:
  awa changes --stat     # what changed since the checkpoint
```

- `checkpoint:` is the baseline everything else is measured against — the same
  label `awa changes` and `awa diff` print in their header. With no checkpoint
  yet, it says so and suggests creating one.
- `dirty:` is either `clean since checkpoint` or a count with the per-kind
  breakdown: added, modified, deleted, renamed, type-changed, skipped.
- `git:` is `branch <name> @ <short-commit>` with the worktree state, or
  `unavailable (<reason>)`, or `non-git`. The dashboard always states git status
  so it is obvious whether a git cross-check applies at all.
- `reusable:` counts runs replayable for the current observed state. When none
  are, it reports how many near misses exist and points at `awa run ls --near`;
  a `nearest:` sub-line then names the closest candidate and its reason token.
  When that reason is `effect-state-differs` or `effect-state-unavailable` *and*
  the run's state was compared against an observation that identified a dominant
  watched root, one further `effect:` line names that root. Otherwise the reason
  token is the whole answer: a run recorded as non-reusable back when it ran keeps
  its reason without a root, because today's dominant root says nothing about that
  earlier execution, and repeating the token as prose would add nothing. `--json`
  still carries the rootless diagnosis with its sample fact and typed actions.
  Effect state has no changed-path sample, and the full exclude / effect-root /
  `awa run --record` decision stays with the `awa run` footer.
- `next:` lists concrete follow-up commands chosen from the current state. In
  `--json` each entry also carries a `kind`, a `reason`, and a tokenized `argv`,
  so an agent can act on it without parsing the display text.

## Notes go to stderr

Two advisories are written to stderr, not stdout: the git-freshness note (the
baseline predates or diverges from HEAD) and the closing review-coverage note.
Redirecting stdout therefore leaves the dashboard body clean, and a script that
captures only stdout does not have to filter them out.

## What status deliberately does not do

`awa status` is bounded on purpose, and it distinguishes "nothing" from
"unknown":

- `checkpoints: 0 readable` is not `none yet`. The first means records exist but
  could not be read; the second means none were ever created.
- It does not walk the blob store, so it reports no footprint at all. The closest
  figure is `awa gc --dry-run --json`, and even that reports accounted bytes
  rather than physical space — an undecodable entry is still reclaimable but its
  size is never read. For a true on-disk size, measure `.awa/` with the operating
  system.
- It samples the newest runs rather than validating every stored record, so the
  corrupt and incompatible counts are qualified by that sample. It is not the
  exhaustive diagnosis; use `awa doctor --json` for that, which the `next:`
  block suggests as soon as any degraded evidence appears.

## The dashboard is not a completed review

`dirty:` reports what changed since the checkpoint baseline, which is not the
same set of changes as the full uncommitted git diff. A `clean since checkpoint`
line means only that nothing moved since that baseline. Before accepting work,
inspect the full uncommitted worktree with the project's normal review tools —
which is what the closing note on stderr says.

## Machine form

`awa status --json` carries the same facts plus a `review` object with the
checkpoint, dirty summary, git state, run counts, typed `next` entries, the
baseline freshness token, and the review-coverage note. Timestamps are UTC.
Degraded evidence appears as entries in `degradations` and `warnings` rather
than as missing fields. The `effect:` detail above is not a separate status
field: it is `review.runs.nearest.effect`, the same optional
`{reason, root?, sample, actions}` object `awa run --json`, `awa run ls --near
--json`, and `awa run explain --json` emit for an effect-state miss.

## See also

- [awa help workflows](workflows.md)
- [awa help diff](diff.md)
- [awa help run](run.md)
- [awa help doctor](doctor.md)
- [awa help json](json.md)
