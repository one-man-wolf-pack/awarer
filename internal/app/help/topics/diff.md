# awa changes / awa diff

```text
awa changes [range] [path...]
awa diff    [range] [path...]
```

Both compare two states. `changes` summarizes what differs (added/modified/
removed, optionally `--stat` or `--name-only`); `diff` shows the textual diff.
If no range is given, both use `latest..now` (see [awa help refs](refs.md) for
the range syntax).

```text
awa changes                       # latest..now, summary
awa diff @-2..@-1                 # between two checkpoints
awa diff -2                       # shortcut for @-2..now
awa changes <checkpoint-id>..now
```

`<checkpoint-id>` is the immutable id `awa checkpoint` printed; any unambiguous
leading portion of it works too.

An empty changes/diff delta means only "no changes since that baseline" — never
that the whole worktree is reviewed. Use the delta to verify a focused fix, then
inspect the full current uncommitted worktree before final acceptance.

## Observation delta basis

Both commands compare two recorded observation manifests. `added`/`deleted`
mean an entry is in only one manifest, not that a file was created or removed;
`modified`, type and rename facts describe the recorded entries. Counts cover
the selected paths only.

The `basis:` header line (JSON `basis`) names this and each side's recorded
scan configuration identity — include/exclude config, ignore-file switches,
symlink and hashing policy, trust mode. When they differ, one `warning:` goes to
stderr (stdout, including `--name-only`, is unchanged) and the comparison still
completes:

- Add `gen/` to `[scope].extra_excludes` after a checkpoint: `gen/` files show as
  deleted while still on disk, and the identities differ.
- Add `gen/` to `.gitignore` or `.awaignore` instead: the same files show as
  deleted, yet the identities match — the identity records the config, not
  ignore-file contents.

So matching identities do not prove unchanged ignore rules, equal scope, or
that a path is absent. Byte-retention settings are not part of the identity:
they only decide whether a side can show a text diff. The differing settings
themselves are not retained, so the warning says their cause is unavailable
rather than guessing one. A run observation (`run:<id>:before`/`after`) uses
the run scope, while checkpoints and `now` use the history scope — which also
hides `dist`, `build` and `coverage` — so their identities can differ with no
configuration change.

## Missing side of added/deleted rows

Each `A`/`D` row is qualified by the side that lacks it — the left observation
for an add, the right for a delete — from that side's recorded coverage:

```text
D gen/a.txt (right: outside observation)   # excluded or pruned there
D old.txt (right: coverage unknown)        # record predates coverage evidence
A cache/x (left: skipped boundary)         # under a skipped/unreadable node
D keep.txt                                 # proved absent in that observation
```

None of these is a filesystem event: a proved absence does not mean the file
was created or deleted in between, and `outside observation` neither says why
the path was excluded nor that it exists. A recovery observation
(`restore:<id>:before`) proves absence only at the paths its restore covered;
everything else is outside its observation. `coverage unknown` is a valid fact
about an older record; damaged or unreadable proof fails the command instead.
`--stat` and `--name-only` keep their counts and names and add one stderr
`note:` counting rows whose missing side is not proved absent.

JSON `scope.ignore_sources` names what today's configuration enables, not what
either side was scanned with: an enabled `.gitignore` may not have existed then,
or now. Each side's actual ignore sources are reported as `unknown`.

## Git context

When the project is a git repo, `changes`/`diff` name the current git HEAD and
add a "note:" when the baseline checkpoint predates or diverges from it — the
baseline is still valid (useful for archaeology or a long review), but the delta
may not map onto current HEAD, so review the full git diff before accepting.
`awa log --all` interleaves git commit boundaries between awa records as context
markers; a commit boundary is not an awa state reference and cannot be used in a
range.

## Summary forms

```text
awa changes --stat        # one-line change-count summary
awa changes --name-only   # changed paths only
awa diff --stat           # the same summary for a content diff
awa log --oneline         # compact checkpoint listing
```

These are human output modes. Combining any of them with `--json` is a usage
error, because the JSON document already carries the summary and the paths — see
[awa help json](json.md).

Rename detection is on by default and is bounded: past its limit, comparison
reports the pair as a delete plus an add rather than spending unbounded time.
`--no-renames` turns it off explicitly.

## Path filters

Path filters restrict the comparison output to the given paths; they are
resolved relative to your current directory and normalized to the project root.
They filter output only — they do not create partial states.

## Scope caveats

A comparison can only report what was scanned. Paths excluded by the ignore
rules are not observed at all, so a change confined to them shows as no change
here; see [awa help ignores](ignores.md). Inputs the scan could not read are
counted as skipped and reported rather than silently dropped.

A checkpoint delta is also not the uncommitted git diff: it is measured from the
baseline you chose, which may be older or newer than HEAD.

## Diff algorithm

`awa diff` defaults to the histogram algorithm; pass `--algorithm myers` for
Myers. Selecting two different algorithms is a usage error. Binary or oversized
files are compared hash-only (changed/unchanged) rather than shown as text.

The project-wide default lives in the one-key `[diff]` config table, and the
default hunk context in `[checkpoint].diff_context`; `--algorithm <name>` and
`--context <n>` override them for a single invocation. Neither choice changes
what is compared, only how the comparison is rendered — see
[awa help config](../reference/configuration.md).

Run observations can be used as state references (`run:<id>:before` /
`run:<id>:after`). When one side has no stored content, `diff` degrades to a
hash-only comparison for the affected paths.

## See also

- [awa help refs](refs.md)
- [awa help checkpoints](checkpoints.md)
- [awa help record](record.md)
- [awa help workflows](workflows.md)
- [awa help ignores](ignores.md)
