# scan inputs and ignore rules

awa scans the worktree for two purposes: history (checkpoints, changes, diff)
and run input (the files a wrapped command could read). Both honor these
production defaults:

- `.awa/` and `.git/` are always protected and never scanned; ordinary config
  cannot re-include them.
- Baseline excludes (build/dependency caches such as `node_modules`, `target`,
  `__pycache__`, `.venv`, and similar) apply to both history and run input.
- History-only excludes (`dist`, `build`, `coverage`) apply to
  checkpoints/diff but NOT to run input — a build or test must still see them,
  so they do not affect run cache keys.
- `.awaignore` is ON by default (awa's native ignore source).
- `.gitignore` is OFF by default. It answers "what should not be committed",
  not "what can affect a command", so a gitignored file is still a real run
  input.

Because `.gitignore` is off by default, `awa run` sees files git ignores unless
an awa rule (protected path, baseline exclude, or `.awaignore`) excludes them.

## Which layer am I looking at

`.awa/` and `.git/` are a hard boundary: no rule or scope re-includes them.
Every other exclusion is an ordinary rule, applied from lowest to highest
priority, and the last matching rule decides:

1. **Baseline excludes** — dependency and cache directories, for both history
   and run input.
2. **History-only excludes** — `dist`, `build`, `coverage`. Hidden from
   checkpoints, changes, and diff; never hidden from run input, because a build
   or test must still see them.
3. **Config excludes** — `extra_excludes` in `[scope]`, then in `[history]` or
   `[run]` for that family.
4. **`.gitignore`**, when enabled — from the project root down to deeper
   directories.
5. **`.awaignore`** — from the project root down to deeper directories.

A higher source wins whatever its depth: a root `.awaignore` rule overrides a
nested `.gitignore`. Within one source a deeper file overrides a shallower one
and a later line an earlier one. Baseline and config excludes are therefore
defaults, not a privacy boundary: `!keep.log` in `.awaignore` re-includes a file
that `extra_excludes = ["*.log"]` excluded. To keep a file's bytes out of
checkpoints whatever the scope, see [awa help privacy](privacy.md).

An explicitly narrowed scope outranks excludes for the narrowed subtree: if you
ask for a specific path with `scope.include` or `--scope`, you get it. The
default scope of the whole project does not have that effect.

The built-in layers are not listed in any config file, because they are product
defaults rather than project policy. To see the lists that are actually in force,
including the built-ins and which layer each value came from:

```text
awa config effective
```

## Why .gitignore is not the input boundary

The two questions are different. `.gitignore` answers "what should not be
committed"; awa's run scan must answer "what can affect this command". Generated
files, local fixtures, and secrets are routinely gitignored and routinely change
what a command does, so keying on git's answer would produce false cache hits —
exactly the failure the cache is designed to avoid.

Turning `.gitignore` on (`use_gitignore` in `[scope]` or `[run]`) is therefore a
deliberate decision to stop keying on those files, not a tidiness setting.

## Ignored paths are outside the evidence

An excluded path is not scanned, so its content is not observed: it has no
entry in a checkpoint or in a recorded run's before/after states, it is not in
changes or diff output or a run input key, and `awa` cannot restore it. What a
record does keep is the name of each excluded entry the scan met, as the
boundary where observation stopped. Nothing below a pruned directory is listed,
but those names can themselves be sensitive (see
[awa help privacy](privacy.md)).

That is the intended trade, but it has a consequence worth stating plainly: a
change confined to ignored paths is no change in `awa changes` and `awa diff`.
When that matters — reviewing generated output, or checking what a formatter
touched — either narrow the scope explicitly to include those paths, or use the
project's own tooling instead of a checkpoint delta. Watched effect roots are
observed separately for run reuse, as described below.

<!-- awa:include effect-vs-exclude -->

<!-- awa:include pattern-semantics -->

## See also

- [awa help run](run.md)
- [awa help config](../reference/configuration.md)
- [awa help diff](diff.md)
- [awa help privacy](privacy.md)
- [awa help troubleshooting](troubleshooting.md)
