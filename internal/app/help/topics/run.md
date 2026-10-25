# awa run — deterministic run cache

```text
awa run [flags] -- <command> [args...]
```

`awa run` executes a command and records its output and observed state. A clean
run that left that state unchanged becomes reusable: a later run with the same
key replays the stored result instead of executing the command.

## Result model

- `hit` — a stored result was replayed; the command did not execute.
- `miss` — the command executed; the result may be stored if cacheable.
- `uncached` — the command executed but the result was not made reusable
  (e.g. skipped inputs without `--allow-skipped-inputs`).

A hit or miss returns the wrapped command's own exit code. Stdout and stderr are
captured separately; their interleaving is neither recorded nor promised.
Outside `--json`, `--display full` writes each stream's exact bytes, complete
stdout before complete stderr; other displays and JSON claim no full byte
replay.

## Cache key

The key covers the command, the run input scan (files the command could read),
the working directory relative to the project root, and the relevant
environment. The same command started from another directory is a different
key, so make the directory explicit:

```text
awa run --cwd ./packages/api -- npm test
```

## Trust mode

The trust mode sets how closely the input scan compares files and is part of
the key. Set it with `[hashing].trust_mode`, or per invocation with the global
`--trust-mode <mode>` or the `--strict` shorthand:

- `normal` — the default balance of hashing and stat signatures.
- `strict` — always hash content, never trust size and modification time. Use
  it when a file may change without its metadata changing.
- `fast` — stat-only comparison. It can miss a same-size, same-mtime rewrite, so
  its runs are recorded but never reusable (`fast-trust-mode`).

## Mutation guard

A mutating or failed execution is still recorded as history. Only a clean run
that left its required observed state unchanged — inputs and watched roots,
minus what a matched validation policy waives — becomes reusable. The cache
prefers a false miss (re-run unnecessarily) over a false hit (skip work that
mattered).

## Effect observation

`awa run` also watches generated-output roots with a bounded stat signature:
every *baseline* exclude, such as `node_modules` and `target`; `build`, `dist`,
`out`, and `coverage`, which the input scan still sees; and any
`[run].extra_effect_roots`. `.awa/` and `.git/` are neither scanned nor watched.

Deleting or changing watched state after a reusable run misses
(`effect-state-differs`); a root that cannot be observed safely is never
reusable (`effect-state-unavailable`). An exclude you add yourself is not
watched: see "Effect roots vs excludes" below. A hit covers observed inputs and
the project-local observation policy; by default not files outside the project
root, and never network services, clocks, or home-directory config.

## Latency diagnostics

A note appears only for a cause awa can name, past the shared interactive
threshold, at most two per invocation. Neither cause is a misconfiguration or
can be disabled:

- `large-effect-root` (`run.effect-observation`, hint `record-mode`) — a watched
  generated-output root dominated state observation.
- `full-input-rehash` (`run.input-observation`, hint `review-run-scope`) — reading
  the inputs was slow enough to notice; a hit pays it too.

Both appear on `run`, `run explain`, `run ls`, and `status` (JSON:
`data.diagnostics.performance[]`). Note and hint text, count meanings, and the
safe response are in [awa help troubleshooting](troubleshooting.md).

## Display and flags

`--display` changes only this invocation's terminal output, never capture, the
key, or the exit code; the other flags below change execution or caching:

```text
--display full            default; stream/replay the full output
--display summary         metadata plus a short excerpt
--display tail:<n>        the last n lines
--display none            awa diagnostics only, no command output
--refresh                 ignore any existing hit, publish a fresh result
--no-cache                neither read nor write the cache
--no-cache-failures       execute, but do not cache a failed result
--allow-skipped-inputs    allow caching when the input scan skipped files
--cwd <path>              set the execution directory (part of the key)
```

## Stdin

A terminal or the null device gives the command no input. `--allow-tty` passes
the terminal, and piped or redirected input is passed through; both are unkeyed,
so the run is not reusable (`stdin-not-keyed`).

`--stdin null` gives the command EOF whatever awa's input is, and keys the run
as if its input were the null device; other reuse conditions still apply. Use it
only when the command reads no input. awa leaves its own input unread, so an
upstream producer may block or get a broken pipe. Not with `--allow-tty`.

## Footer and inline diagnosis

Every execution and hit ends with a footer: storage state
(hit/stored/record-only/uncached), run id, cwd, exit, duration, reusability, and
the inspect command, plus the before/after compare for a non-reusable run. A
common miss adds a bounded preview: the nearest previous run, why it is not
reusable, a sample of changed inputs, and the `run explain` / `run ls --near`
follow-ups.

## Why a run is not reusable

The footer, `run ls`, `run explain`, and stored runs name the cause with a
closed token; switch on the token, not on prose. Validation-policy and
identity-probe causes (`external-facts-*`) are in
[awa help validation](validation.md).

The observed state no longer matches what was recorded:

```text
input-tree-differs        the input tree changed (the only one with a path sample)
mutated-state             the command changed observed state
effect-state-differs      watched generated output changed or was deleted
effect-state-unavailable  a watched root could not be observed safely
fast-trust-mode           observed under fast trust; recorded, never reusable
```

A recorded key fact no longer matches:

```text
expired                   older than the run TTL
env-mismatch              an allowlisted environment fact differs
cwd-mismatch              the execution directory differs
config-mismatch           another keyed fact differs (scope, trust mode, ...)
platform-mismatch         recorded on a different OS or architecture
```

The stored evidence cannot be replayed honestly:

```text
payload-missing           the stored output is absent or fails its hash
corrupt                   the stored metadata could not be decoded
post-scan-failed          the post-run observation scan failed
```

Policy declined to make the run reusable:

```text
non-cacheable-policy      caching was forbidden for this invocation
record-only               captured as supervised history, never offered
no-cache                  ran without reading or writing the cache
stdin-not-keyed           piped or tty stdin is not part of the key
skipped-inputs            the scan skipped inputs without the allow flag
capture-disabled          output capture was off, so nothing was stored
failed-not-cached         the command failed and policy declines to cache it
```

## Inspect and explain

```text
awa run ls                    runs reusable for the current state
awa run ls --near             also recent near misses: reason, changed-path sample
awa run log                   chronological run history, newest first
awa run show --last           a run's metadata and captured output
awa run explain -- <command>  the cache decision, without running
```

None of these runs identity probes. If [validation policy](validation.md)
probes are approved but have not run, explain says `incomplete` with no key
unless another rule decides, and `run ls` lists the run as unassessed: unknown,
not a miss. With `--json`, `run ls` and `run explain` share one near-miss shape;
see [awa help json](json.md).

## Evidence and privacy

Captured stdout/stderr is stored verbatim and never redacted, so a printed
secret is stored. Output over the capture limit is stored truncated, and the
footer, replay, and `run show` state the omitted byte count. The child does not
inherit your environment: it gets the built-in baseline (your locale
unchanged), what `env_allowlist` adds, and the advisory marker `AWA_RUN=1`, all
keyed; allowlisted values are stored only as a redacted identity. See
[awa help privacy](privacy.md).

<!-- awa:include effect-vs-exclude -->

## See also

- [awa help validation](validation.md)
- [awa help record](record.md)
- [awa help refs](refs.md)
- [awa help ignores](ignores.md)
- [awa help config](../reference/configuration.md)
- [awa help privacy](privacy.md)
