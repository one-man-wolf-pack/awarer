# JSON output, exit origin, and script rules

`--json` turns a command's human report into a stable machine document. Use it
for anything automated: the human text is allowed to change wording, the JSON
shape is not.

## The envelope

Every JSON payload is wrapped the same way:

```text
{ "schema_version": 1, "command": "<name>", "data": { ... } }
```

`<name>` is the command that produced it (`status`, `changes`, `run.show`, ...).
The envelope's `schema_version` versions the envelope itself. Provider contracts
carried inside `data` — `awa-state/v1` for resolved state, `awa-evidence/v1` for
run evidence — are versioned independently and appear as their own
`provider_contract` fields.

Payload goes to stdout. Diagnostics, notes, hints, and errors go to stderr, so a
script can capture stdout alone and get only the document.

## Which commands accept --json

Global options are honored per command, not universally: a command that does not
act on `--json` rejects it as a usage error rather than accepting and ignoring
it. The exhaustive list is in [global options](../reference/global-options.md) and
the per-command pages under [commands](../commands/index.md).

One case worth stating: `awa docs export` has no `--json`. The export writes its
own machine contract, `manifest.json`, into the output directory.

## Rules for scripts

Four rules make the difference between a robust consumer and one that
occasionally believes a truncated document:

1. **Parse stdout only after the process exits.** Streamed payloads are written
   as they are produced.
2. **A non-zero exit does not mean the document is invalid.** For `awa run` the
   exit code is usually the wrapped command's own; the envelope is still one
   complete document.
3. **Treat a `partial output:` line on stderr as fatal for the document.** It
   means the report failed part-way through.
4. **Never salvage a partial document.** Discard stdout and re-run; do not
   reconstruct the missing tail or parse the fragment that arrived.

Streaming surfaces such as `awa changes --json` and `awa diff --json` emit their
entries incrementally, so a mid-stream failure leaves the document deliberately
unterminated — it cannot parse as complete, which is the intended signal, paired
with the `partial output:` diagnostic and a non-zero exit.

## Exit origin

`awa run` returns the wrapped command's own exit code, and that code can overlap
awa's own codes in the range 1–6. `$?` alone therefore cannot tell you who
failed.

The run envelope answers it explicitly: `data.run.exit_origin` is `child`,
alongside `data.run.exit_code` and `data.run.signal`. An awa-origin failure —
invalid configuration, a corrupt store, a lock timeout — is reported through the
error path with an awa exit code and never as a run envelope, so the presence of
the envelope is itself part of the classification. See
[awa help exit-codes](exit-codes.md).

## Human output modes are not JSON modes

`--stat`, `--name-only`, `--oneline`, and `--time` shape human output. Combining
any of them with `--json` is a usage error (exit 2) rather than a silent no-op,
because the JSON already carries the underlying facts. Timestamps in JSON are
always UTC RFC3339 regardless of the configured display mode.

## Bounded and degraded results are labelled

A payload never quietly shrinks. Where a list is capped, the document carries a
completeness fact next to it — `complete`, `shown`, `limit`, and a `reason` —
for example `candidates_bounded` on `awa gc --json`, while the summary still
reports full totals. Similar fields elsewhere state whether a set of paths is
complete, truncated, or unavailable.

Degraded evidence is reported as a complete assessment rather than as an error:
`awa state resolve`, `awa state compare`, and `awa run show --json` exit 0 and
name the situation with a closed reason token (for example `not-initialized`,
`not-found`, `ambiguous-reference`, `permission-denied`). A degradation is always
named that way rather than left for you to infer from what is missing.

Optional keys, on the other hand, are omitted when they have no value: a
checkpoint with no message carries no `message` key, a run with capture disabled
carries no `outputs` block, and a binary with no VCS stamping carries no
`revision`. Read anything a document is not obliged to carry with a
present-or-default access rather than a direct index. What is stable is the
envelope plus the keys a document always carries — `awa run ls --json` always has
its `diagnostics.performance` array, empty when nothing crossed the latency
threshold, so machine-readable latency facts never live only on stderr.

## See also

- [awa help exit-codes](exit-codes.md)
- [awa help status](status.md)
- [awa help run](run.md)
- [awa help inspect](inspect.md)
- [awa help integrations](integrations.md)
