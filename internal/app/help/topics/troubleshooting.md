# diagnosing awa — slow, stale, blocked, or broken

Start from the symptom. Each section below ends at the topic that owns the
detail, so you do not have to read the others.

Two commands answer most questions before anything else:

```text
awa status
awa doctor
```

`awa status` is the bounded dashboard — baseline, drift, reuse, next steps.
`awa doctor` is the exhaustive diagnosis of durable state, which status
deliberately does not run.

## A check I expected to reuse ran again

A miss is a statement about the current inputs, not a malfunction. Ask what
changed:

```text
awa run ls --near
awa run explain -- <command>
```

`<command>` is the same invocation you wanted reused. Both report one token from
the closed reason vocabulary in [awa help run](run.md); the common ones and what
they mean for you:

- `input-tree-differs` — an input the command can read changed. `--near` shows a
  bounded sample of which paths.
- `effect-state-differs` — watched generated output (`build`, `dist`, `target`,
  `node_modules`, ...) changed or was deleted since the run.
- `cwd-mismatch` — `awa run` is cwd-sensitive by design. Pin it with
  `--cwd <path>`.
- `env-mismatch` — an allowlisted environment fact differs.
- `expired` — the entry is older than the configured run TTL.
- `record-only` / `fast-trust-mode` — the run was recorded but was never offered
  for reuse in the first place.
- `skipped-inputs` — the scan could not read some inputs, so the key could not
  honestly cover them.

Do not make a miss disappear by weakening the key. `--refresh` and `--no-cache`
change what this invocation does; they do not make an old result valid for
changed state, and an old successful run is not reusable for state that moved.
Record-only evidence is history, never a cache hit. If a command's point is its
side effect, `awa run --record` is the right tool rather than a forced hit.

## awa feels slow

awa names slowness it can classify and says nothing otherwise, so a quiet command
is a command with no known cause — not a command that was fast. A note appears
only when a stage crosses the interactive threshold, and at most two appear per
invocation. `awa run`, `awa run explain`, `awa run ls`, and `awa status` all
write them to stderr. There are two known causes.

**A large watched generated-output root** (`large-effect-root`, component
`run.effect-observation`) dominating state observation:

```text
note: run state observation took <duration>; effect root "<path>" contains <n> files
hint: use `awa run --record` for evidence-only workflows, or review [run] effect roots
```

A root that could not be fully walked within its budget reports `exceeds <n>
files` instead of an exact count, never a fabricated number.

**Reading the inputs** (`full-input-rehash`, component `run.input-observation`):

```text
note: run input observation took <duration>; hashed <n> files under "." — the run-cache
      identity is read from file content, not from stat signatures, so a rewrite that
      leaves size and timestamps unchanged cannot replay a stale result
hint: review what the run input scope covers with `awa help ignores`; never exclude a
      file the command actually reads — an unobserved input cannot invalidate a hit
```

The count is the exact number of regular files the scan hashed. A miss reads the
inputs twice, before and after the command, and reports the slower pass as one
note; a hit reads them once and reports that. Yes, a hit pays this too: proving a
stored result still matches the worktree means reading the worktree.

In JSON the same facts appear under `data.diagnostics.performance[]` with a
stable `code`, `duration_ms`, the `component` it is attributed to, bounded
`evidence`, and a typed `hint` — `record-mode` for the first cause,
`review-run-scope` for the second — carrying the tokenized argv to run.

This is information, not a misconfiguration to fix by re-running, and neither
cause has a flag that turns it off. The safe responses differ by cause. For a
large effect root: use `awa run --record` where you only want evidence, or review
the configured `[run]` effect roots as a deliberate policy decision. For the
input rehash: review what the input scope covers (`awa help ignores`) and narrow
it only where it is genuinely wider than the command.

Both reviews carry the same warning, and it is the important part. Dropping a
root from observation, or excluding a file the command actually reads, is not a
speedup — it is a decision to stop noticing that kind of change. The result is
not a slower awa or a faster one; it is a hit that replays a result computed from
state that has since moved, with nothing on screen to say so.

## A command exited non-zero

Route by code:

- `2` — usage. Read the message; it names the flag or argument.
- `3` — no project here (or an explicit `--config` file that does not exist).
  Run `awa init`, or point at the project with `--root <path>`.
- `4` — a config layer is present but invalid. `awa config validate` names the
  layer and the key.
- `5` — local state needs an action: corruption, or a finding nothing has
  repaired yet. Go to `awa doctor` and read what it names.
- `6` — a required lock was not acquired in time. Another awa process is
  writing; retry, and if nothing else is running, look for a stale-lock finding
  in `awa doctor`. `[locks].timeout` sets the wait.
- `130` — interrupted before anything authoritative was published. Re-run, but
  read the message first: it names anything the stopped command left in a path
  you gave it, and `awa docs export` leaves a directory you have to remove before
  the retry is allowed.

If the command was `awa run`, the code may belong to the wrapped command rather
than to awa. Use `--json` and read `data.run.exit_origin`, or look the run up
with `awa run log`. Full table: [awa help exit-codes](exit-codes.md).

## The command behaves differently under `awa run`

A wrapped command gets a sanitized environment, not yours: the built-in baseline,
plus `[run].env_allowlist`, plus `AWA_RUN=1`. If behavior differs from a direct
run, a variable the command depends on is almost always the reason.

```text
awa config effective     # effective_env_allowlist = what the child inherits
                         # injected_env            = what awa adds
```

Add the name and rerun:

```toml
[run]
env_allowlist = ["MY_TOOL_MODE"]
```

Encoding, sorting, or message language is the one case you should not need to fix
yourself: the locale variables (`LANG`, `LC_*`, `LANGUAGE`) are inherited and keyed
already, exactly as you have them. If a command still reports an ASCII or `C`
encoding, check that the locale is actually set in the shell that invoked awa —
`awa run` passes what it finds and never invents one.

## Config rejects an env_allowlist name

```text
run.env_allowlist[0] "LANG" is already in the built-in baseline; remove it
```

That name is now inherited and keyed for every run, so listing it does nothing.
Delete the entry from `awa.toml` or `.awa/config.toml`; nothing else changes.

`AWA_RUN` is rejected differently, and permanently: it is awa's own injected
marker, not a variable you can redirect or silence.

## Storage looks damaged

```text
awa doctor
awa doctor --repair
awa doctor --repair --strict
```

Diagnose, then repair. `--repair` performs only safe, mechanical, awa-owned fixes
and never touches your worktree.

`--strict` is not the general escalation for "findings remain". All it changes is
how the checks decide: it recomputes content hashes instead of trusting size and
modification time, and then repairs what that deeper diagnosis found. So it helps
exactly when content may have changed without its metadata changing — after a
crash, after copying the project between machines or filesystems, or on a restored
backup. It cannot resolve a policy warning or a finding that has no repair, and it
is slower, substantially so on a large store.

For anything else that survives `--repair`, read the finding: each one states the
action it invites, and several are deliberately for you rather than for awa (broad
`.awa/` permissions name the `chmod` to run; `.awa/` tracked by git is a commit to
undo). See [awa help doctor](doctor.md) for the finding families.

Note that `awa doctor` exits 5 while any repairable finding is unrepaired, so a 5
before you have run `--repair` does not by itself mean data is damaged.

`awa gc` fails safe in the same situation, and does so wholesale: when corruption
or an active or unknown writer lock makes deletion unsafe it reports those
candidates as blocked and deletes nothing at all — not even the unblocked
candidates. A lock exits 0 (retry later), corruption exits 5. Exit 6 from `gc`
means something else entirely: another `gc` held the exclusive collector lease
past `[locks].timeout`, so retry rather than reaching for `doctor`. If `gc`
reports corruption, run `awa doctor` before anything else.

As a last resort, `.awa/` is local evidence, not source: deleting it loses
checkpoint and run history — and, because it lives inside `.awa/`, your private
`.awa/config.toml` layer. Your worktree, your git history, and a committed
`awa.toml` are untouched, and `awa init` starts a fresh store.

## awa cannot find the project

`awa` walks up from the current directory looking for a `.awa/` directory. If it
reports that this is not an awa project, either you are outside the tree or it
was never initialized. Say where it is, or check which config files are in play:

```text
awa status --root <path>
awa config path
```

## The evidence looks unavailable rather than wrong

For the provider surfaces (`awa state resolve`, `awa state compare`,
`awa run show --json`), an unresolvable reference or an unreadable record is a
*complete assessment*, not a failure: they exit 0 and name the situation with a
closed reason token such as `not-initialized`, `not-found`,
`ambiguous-reference`, `metadata-corrupt`, or `permission-denied`. Read the token
rather than inferring from the exit code. See
[awa help integrations](integrations.md).

## A generator or formatter rewrote files it should not have

Put just those paths back from the checkpoint you were working against, previewing
first:

```text
awa restore <checkpoint-id> -- <path>
awa restore --apply <checkpoint-id> -- <path>
```

If the preview reports blocked operations, it names why — `hash-only-content`
means the source proved the file's identity but stored no bytes, `blob-missing`
means the content was already reclaimed, `policy-incompatible` means the source
was observed under a different scan policy so deletions cannot be justified. See
[awa help restore](restore.md).

## See also

- [awa help doctor](doctor.md)
- [awa help gc](gc.md)
- [awa help run](run.md)
- [awa help status](status.md)
- [awa help exit-codes](exit-codes.md)
- [awa help json](json.md)
- [awa help config](../reference/configuration.md)
