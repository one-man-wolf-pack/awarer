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
