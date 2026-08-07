# awa as a provider for other local tools

Another local tool (an editor, a script, a review assistant) can use awa as an
optional, read-only provider of worktree state and run evidence. Integrate only
through the versioned subprocess contract below — never by reading `.awa/`
directly.

## Subprocess-only, read-only

- Run the awa binary and parse its `--json` output. Do not decode
  `.awa/checkpoints`, `.awa/runs`, manifests, indexes, or pointers; that layout
  is private and changes between releases without notice.
- The provider surfaces are read-only. They never mutate state, create pins or
  leases, or edit another tool's files.

## State identity — `awa state` (provider_contract: `awa-state/v1`)

```text
awa state resolve <ref> --json             # a full immutable state identity,
                                           # or a stable reason it is unavailable
awa state compare <ref-a>..<ref-b> --json  # equal / different (with counts) /
                                           # incomparable / unavailable
```

Refs: `now`, `latest`, `<checkpoint-id>`, `run:<id>:before`, `run:<id>:after`
(see [awa help refs](refs.md)). An expected "unavailable" is a successful
assessment (exit 0), not an error to scrape from stderr.

## Run evidence — `awa run show` (provider_contract: `awa-evidence/v1`)

```text
awa run show <id> --json                 # one run as reference-quality metadata:
                                         # command, cwd, exit, duration, reuse,
                                         # mutation, before/after state identities,
                                         # and output byte/hash/truncation facts
```

`states.before` and `states.after` are complete `awa-state/v1` assessments,
identical to `awa state resolve run:<id>:before|after`. Default JSON is metadata
only: it includes captured stdout/stderr lines only through a bounded
`--tail`/`--grep` sample (`--stdout`/`--stderr` merely pick which stream that
sample covers and need a filter; a raw `--stdout`/`--stderr` dump is a non-JSON
surface). A metadata view never claims payload bytes are intact: inspectability
reports presence, and integrity stays "unverified" until an explicit
`--tail`/`--grep` read opens and hash-verifies that stream, which reports it
"verified" (an unselected stream stays "unverified").

## Provider versioning

Provider versions are independent. `awa-state/v1` versions state identity; the
enclosing `awa-evidence/v1` versions run-evidence composition. Additive detail
may appear; a changed identity field, outcome/reason spelling, or equality
meaning is a version bump. The private `.awa/` storage format is separate from
both and may change freely before release.

## Availability and GC

Provider references are local evidence, not durable task records. Ordinary
`awa gc` may remove a referenced checkpoint or run; a later resolve then returns
unavailable with a stable reason, without rewriting the reference or calling a
deleted record corrupt. Absence may weaken evidence but never blocks Git or
manual review. awa adds no cross-tool pins, leases, callbacks, or retention.

## Privacy

`.awa/` holds real evidence (see [awa help privacy](privacy.md)). Keep it
private and untracked; captured output is stored verbatim, so treat it as
evidence, not sanitized text.

## Excluding another tool's local state

A tool that writes high-churn private state beside `.awa/` — for example
`.rezonator/ledger.sqlite` — is project configuration, not universal awa
knowledge. It is not a built-in exclude. Exclude it explicitly so its writes do
not move checkpoint/current-state identity or invalidate unrelated cached
checks:

```text
echo '.rezonator/' >> .awaignore          # or shared awa.toml scope.extra_excludes
```

After that, a write confined to `.rezonator/` leaves awa state equal, while a
real source edit still moves it and invalidates the relevant checks.

## Commands that read excluded state

If a command's behavior depends on an excluded directory, awa cannot soundly
serve it from reusable cache mode — its inputs are no longer fully keyed. Run
such a command directly, or under `awa run --record` when supervised history is
useful; never weaken the cache key to make it "hit". awa never edits your
`.awaignore`, `awa.toml`, `.gitignore`, or another tool's files.

## See also

- [awa help refs](refs.md)
- [awa help record](record.md)
- [awa help json](json.md)
- [awa help privacy](privacy.md)
- [awa help gc](gc.md)
