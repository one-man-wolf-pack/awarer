# awa run — deterministic run cache

```text
awa run [flags] -- <command> [args...]
```

`awa run` wraps a command. On the first run it executes the command, captures
its output and observed state, and — if the run is clean and non-mutating —
stores a reusable cache entry keyed on the inputs the command could see. A later
run with the same key replays the stored result instead of executing again.

## Result model

- `hit` — a stored result was replayed; the command did not execute.
- `miss` — the command executed; the result may be stored if cacheable.
- `uncached` — the command executed but the result was not made reusable
  (e.g. skipped inputs without `--allow-skipped-inputs`).

Stdout and stderr are captured separately; their cross-stream interleaving is
neither recorded nor promised. Every hit returns the stored child exit. Outside
`--json`, `--display full` writes each stream's exact bytes, complete stdout
before complete stderr; bounded, hidden, and JSON displays claim no full byte
replay.

## Cache key

The cache key covers, at a high level: the command, the run input scan (files
the command could read), the working directory relative to the project root, and
the relevant environment. `awa run` is cwd-sensitive by design — the same
command launched from a different directory is a different key. Make it explicit
with:

```text
awa run --cwd ./packages/api -- npm test
```

## Trust mode

How closely the input scan compares files is the trust mode, set by
`[hashing].trust_mode` and overridable per invocation with the global
`--trust-mode <mode>` (`normal`, `strict`, or `fast`), or with `--strict` as a
shorthand:

- `normal` — the default balance of hashing and stat signatures.
- `strict` — always hash content, never trust size and modification time.
  Slower, and the right choice when a file may change without its metadata
  changing.
- `fast` — stat-only comparison. It can miss a same-size, same-mtime rewrite, so
  a run observed under it is recorded but never published as reusable
  (`fast-trust-mode`).

Trust mode is part of the cache key, so changing it does not silently reuse
results observed under a different one.

## Mutation guard

`awa run` returns the wrapped command's own exit code after a hit or a miss; a
mutating or failed execution is still recorded as history, but only a clean,
non-mutating run becomes a reusable hit. The cache prefers a false
miss (re-run unnecessarily) over a false hit (skip work that mattered).

## Effect observation

`awa run` also watches generated-output roots with a bounded stat signature. The
watched set is the built-in one — every *baseline* exclude, such as `node_modules`
and `target`, plus `build`, `dist`, `out`, and `coverage` — plus any
`[run].extra_effect_roots`, selecting by name at any depth or by exact
project-relative path. That second built-in group is watched though the input scan
still sees it: a build artifact is both a real command input and generated state.
`.awa/` and `.git/` are the exception: neither scanned nor watched, because awa's
own state and your VCS metadata are outside its guarantee.

Note the boundary, because it is the one place excluding a path can cost you a
correct result: **an exclude you add yourself is not watched**, so a replay can
report success while an excluded, unwatched directory is missing. "Effect roots
vs excludes" below turns that into a decision.

Deleting or changing that generated state after a reusable run misses instead of
falsely hitting (reason `effect-state-differs`); a watched root that cannot be
observed safely, or a run under the fast trust mode, records but never becomes
reusable (`effect-state-unavailable`, `fast-trust-mode`). A cache hit is reusable
for observed inputs and configured project-local observation policy; by default
it does not track files outside the project root, and never network services,
clocks, or home-directory config.

## Latency diagnostics

awa explains slowness it can name and says nothing otherwise: a note appears only
past the shared interactive threshold, at most two per invocation. Neither cause
is a misconfiguration; neither is disableable.

- `large-effect-root` (`run.effect-observation`, hint `record-mode`) — a watched
  generated-output root dominated state observation.
- `full-input-rehash` (`run.input-observation`, hint `review-run-scope`) — reading
  the inputs was slow enough to notice; a hit pays it too.

Both appear on `run`, `run explain`, `run ls` and `status` (JSON:
`data.diagnostics.performance[]`). For the note and hint text, what each count
means, and the safe response, see `awa help troubleshooting`.

## Display

Display affects this invocation's terminal output only — never capture, the
cache key, or the exit code:

```text
--display full        default; stream/replay the full output
--display summary     metadata plus a short excerpt
--display tail:<n>    the last n lines
--display none        awa diagnostics only, no command output
```

## Useful flags

```text
--refresh                 ignore any existing hit, publish a fresh result
--no-cache                neither read nor write the cache
--no-cache-failures       execute, but do not cache a failed result
--allow-skipped-inputs    allow caching when the input scan skipped files
--cwd <path>              set the execution directory (part of the key)
```

## Stdin

By default a terminal or the null device gives the command no input;
`--allow-tty` passes the terminal, and piped or redirected input is passed
through — both unkeyed, so not reusable (`stdin-not-keyed`).

`--stdin null` gives the command EOF whatever awa's input is: an execution
choice, not a cache switch. Use it only when the command needs no input. Its key
is that of a run whose input already is the null device; other reuse conditions
still apply. awa leaves its input unread, so an upstream producer may block or
get a broken pipe; pipeline success is not promised. Not with `--allow-tty`.

## Footer and inline diagnosis

Every real execution and hit ends with a compact footer: storage state
(hit/stored/record-only/uncached), run id, cwd, exit, duration, reusability, and
the inspect command; a non-reusable run also prints the before/after compare.

For a common miss the footer adds a bounded inline diagnosis — the nearest
previous run, why it is not reusable, a sample of changed inputs, and the exact
`run explain` / `run ls --near` follow-ups — a preview, not a replacement.

## Why a run is not reusable

Every surface that reports reuse — the footer, `run ls [--near]`, `run explain`,
and a stored run's reuse state — names the cause with one token from this closed
vocabulary, grouped by what the token is about.

The observed state no longer matches what was recorded:

```text
input-tree-differs        the observed input tree changed
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

Only `input-tree-differs` can produce a changed-input-path sample, because only
that comparison has two input manifests to diff. The tokens are stable machine
values: switch on them rather than on the surrounding prose.

## Inspect and explain

```text
awa run ls                current-state reuse view (reusable right now)
awa run ls --near         also list near misses and why they are not reusable
awa run log               chronological run history, newest first
awa run show --last       inspect a run's metadata and captured output
awa run explain -- <command>  compute the key and cache decision without running
```

`run ls --near` adds, for each recent non-reusable run, its reason and a bounded
sample of changed input paths; `run explain` reports the same reason.

Add `--json` to any of these for a stable machine form; `run ls --json` and
`run explain --json` report the same facts and share one near-miss shape. See
[awa help json](json.md).

## Evidence and privacy

Captured stdout/stderr is stored verbatim as durable evidence, so a command that
prints a secret stores it — awa does not redact output. Allowlisted env values
are keyed and passed to the child but stored only as a redacted identity, never
raw. Output over the capture limit is stored truncated, and every surface
(footer, replay, `run show`) states the omitted byte count.

The child does not inherit your environment: it gets the built-in baseline (your
locale included, unchanged), what `env_allowlist` adds, and the fixed advisory
marker `AWA_RUN=1` — all keyed. See [awa help privacy](privacy.md).

<!-- awa:include effect-vs-exclude -->

## See also

- [awa help record](record.md)
- [awa help refs](refs.md)
- [awa help ignores](ignores.md)
- [awa help config](../reference/configuration.md)
- [awa help privacy](privacy.md)
