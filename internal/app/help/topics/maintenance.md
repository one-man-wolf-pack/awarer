# maintaining an existing project — setup review and cleanup decisions

Use this page when awa is already set up in a project and you are asked whether
that setup is right: what it scans, what it keeps, why checks hit or miss, and
whether anything should be changed or removed. It is a reading path, not a
certification. Every step below only inspects; a change, a repair, or a deletion
is a separate decision you make and justify afterwards.

## Inspection path

```text
awa help config                # config schema, precedence, and when to change it
awa doctor                     # integrity and privacy findings, store size
awa doctor --capacity          # also the size of watched generated-output roots
awa config validate            # every config layer parses and is accepted
awa config effective           # the values actually in force, and their origin
awa status                     # baseline, drift, and reuse at a glance
awa run log -n 20              # recent recorded executions, newest first
awa run ls --near              # reusable now, plus near misses and their reason
awa run explain --last         # why the latest run is or is not reusable
awa run show <id> --meta       # one run's command, cwd, exit, and reuse state
awa gc --dry-run               # what explicit collection would remove, and why
```

Between `config effective` and the run history, read the project's own commands:
the files each one reads, the files and directories it writes, and the
environment variables it needs. awa cannot infer those for you, and every setting
below is a statement about them.

The `normal` trust mode is enough for this review; `--strict` is for suspected
metadata-preserving edits or a crash, not a routine step. Neither
`awa doctor --repair` nor any deletion belongs to the inspection itself.

## What doctor ok does not tell you

`awa doctor` reports on the checks it runs: store integrity, locks, the `.awa/`
git guard, permissions, and a bounded set of privacy hints. A clean result says
those checks found nothing. Its `store:` line is the logical size of `.awa/` under
a bounded walk; a `store-capacity-threshold` warning says that size reached
`doctor.store_warning_size`, not that anything is damaged or safe to delete.
`--capacity` sizes only the directories the effect-root selectors name, within its
bounds; it is not a disk report (use `du` or similar), and neither doctor nor `gc`
removes project output. A clean doctor result does not say the input scope is
complete, that every environment variable a command reads is allowlisted, that the
cache strategy suits the project, or that stored logs and older checkpoints are
free of secrets. Treat it as one input to the review, never as its conclusion.

## Inputs, history, retention, and effects are different choices

Four settings look alike and answer different questions:

- **Run input scope** (excludes for run, `.awaignore`) — what a command's key is
  computed from. Excluding a path a command reads makes it invisible to the key.
- **History scope** (history excludes) — what checkpoints and `awa changes` show.
  It does not shrink run input.
- **Hash-only retention** (`[checkpoint] hash_only_patterns`) — a file is still
  scanned and still a run input; new checkpoints keep its hash, not its bytes.
- **Effect observation** (`[run].extra_effect_roots`) — a bounded watch on
  generated directories, so deleting or changing them misses instead of hitting.

A secret file a command still reads, such as a local env file, stays an input:
keep its bytes out of checkpoints with hash-only retention rather than excluding
it, which would also hide its changes from the key.

A directory that run input already sees needs no effect root: its contents are
in the key. The risky combination is an excluded dependency nobody watches — a
read-only consumer of it, such as a test reading generated output, can replay a
stale success after that output changes or disappears. Watch such a directory or
leave it visible. And a replay prints the stored output; it does not recreate
files a build wrote.

Generated, vendored, and agent-owned directories have no universal answer. Decide
per directory from what the project's commands do with it: input, output, both,
or neither. A command that mutates the worktree belongs under
`awa run --record`; making it look cacheable by hiding what it writes trades a
miss for a false hit.

## Environment the commands consume

Allowlist the variables the project's commands actually read, by name, in
`[run].env_allowlist`, and no more. The inherited baseline — including `PATH`,
`SHELL`, and the locale variables — is keyed on purpose: they select which
executable runs and how it reads and writes text. A miss caused by a different
`PATH` or locale is the cache being honest, not a defect to remove; do not narrow
keys so that different environments share hits.

The run record keeps a redacted identity of each environment value it passes,
not the value. That protects the environment only: argv, stored file bytes, and
captured stdout/stderr are kept verbatim. A command that receives a secret on
its command line or prints it has stored it. See [awa help privacy](privacy.md).

## Recorded runs are evidence, not a hit rate

A record-only or non-reusable run is still useful: it keeps what ran, its exit,
its output, and the before/after state. Do not count records to judge the cache.
A hit replays a stored result and creates no new execution record, so history
counts executions, not hits; and a run that was reusable when stored need not be
reusable now. Ask the current question instead:

```text
awa run ls --near                         # what is reusable against this state
awa run explain --last                    # one run's reuse decision and reason
awa run explain --from-run <id> --to-now  # a stored run's inputs against now
awa run explain -- <command>              # the key and decision, without running
```

The reason tokens are listed in [awa help run](run.md). Read them per command.
`input-tree-differs` usually means the inputs really changed; read its path
sample before calling it a scope problem, and never exclude a real input to get
hits. A repeated `env-mismatch` points at the environment, while `mutated-state`
or `record-only` usually means the command is not a cache candidate at all.

## Commands that need no input

An inherited pipe or regular file is passed to the command and makes the run
non-reusable, because those bytes are not keyed. A terminal or the null device
(`</dev/null`) gives the command end-of-file by default and does not by itself
prevent reuse. `--allow-tty` passes the terminal through and makes the run
non-reusable. When a command genuinely needs no input but awa's own input is a
pipe or file, `--stdin null` gives it end-of-file deliberately. That is a choice
about what the command receives, not a promise of a hit: every other reuse
condition still applies, so never use it to silence a command that reads stdin.
Upstream pipeline effects and the exact option are in [awa help run](run.md).

Evidence is per project root. A run supervised from one checkout observes that
checkout; it says nothing about edits in another worktree the command happened
to read or write.

## Retention and the gc preview

Retention settings in `[gc]` make records eligible for collection; nothing
collects in the background. Space comes back only through an explicit `awa gc`
or `awa run rm`.

`awa gc --dry-run` is a proposal for that explicit deletion: the candidates it
selected, what it retains, and what blocks it. It is not a report of the whole
store's size (that is doctor's `store:` line), and it is not a secret sanitizer —
it removes only what retention allows. Its byte figures are logical file lengths
for the selected candidates, can be a lower bound, and need not match disk usage. The workflow and every
reason token are in [awa help gc](gc.md).

## What a config change does not undo

New excludes, new hash-only patterns, or a narrower allowlist apply to future
observations. Existing checkpoints keep the bytes they stored, and stored run
output keeps whatever was printed, until those records are collected or deleted.
Hash-only retention also has a cost: newer checkpoints cannot show a text diff of
those files or restore their bytes. A stored hash is not secret protection; a
short or guessable value can be confirmed against it.

## Decisions that need an explicit choice

Propose, with the evidence that justifies it, and let the owner decide:

- a config change — which layer, which key, and the command behavior it
  reflects ([config reference](../reference/configuration.md),
  [awa help ignores](ignores.md));
- `awa doctor --repair` — only for the findings it names
  ([awa help doctor](doctor.md));
- deleting evidence — the narrowest tool that covers it: `awa run rm` for
  specific runs ([awa help inspect](inspect.md)), or `awa gc` after reading its
  preview;
- a leaked credential — rotate it first; then remove the records that hold it
  with the narrowest tool that reaches them. Deleting `.awa/` wholesale is a
  last resort that also takes the private local config with it
  ([awa help privacy](privacy.md)).

## See also

- [awa help config](../reference/configuration.md)
- [awa help ignores](ignores.md)
- [awa help run](run.md)
- [awa help record](record.md)
- [awa help inspect](inspect.md)
- [awa help privacy](privacy.md)
- [awa help doctor](doctor.md)
- [awa help gc](gc.md)
