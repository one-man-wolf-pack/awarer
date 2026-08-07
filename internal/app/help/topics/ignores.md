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

The layers apply in this order, and later ones cannot re-include what an earlier
one protects:

1. **Protected** — `.awa/` and `.git/`. A hard boundary; no configuration
   overrides it.
2. **Baseline excludes** — dependency and cache directories, for both history
   and run input.
3. **History-only excludes** — `dist`, `build`, `coverage`. Hidden from
   checkpoints, changes, and diff; never hidden from run input, because a build
   or test must still see them.
4. **User rules** — `.awaignore`, plus `extra_excludes` in the `[scope]`,
   `[history]`, and `[run]` config sections, which are additive per family.

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

An excluded path is not scanned, so it is not observed. Nothing awa records
describes it: it is absent from checkpoints, from changes and diff output, from
run input keys, and from a recorded run's before/after states. `awa` makes no
claim about it and cannot restore it.

That is the intended trade, but it has a consequence worth stating plainly: a
change confined to ignored paths looks like "no changes" in every awa surface.
When that matters — reviewing generated output, or checking what a formatter
touched — either narrow the scope explicitly to include those paths, or use the
project's own tooling instead of a checkpoint delta.

<!-- awa:include effect-vs-exclude -->

<!-- awa:include pattern-semantics -->

## See also

- [awa help run](run.md)
- [awa help config](../reference/configuration.md)
- [awa help diff](diff.md)
- [awa help privacy](privacy.md)
- [awa help troubleshooting](troubleshooting.md)
