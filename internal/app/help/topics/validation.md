# awa run validation policies — result-only replay

```toml
[[run.validation]]
name = "cargo-test"              # a label for output; renaming it keeps cached results
argv = ["cargo", "test"]         # the complete command, element for element
cwd = "."                        # the exact execution directory
unobserved_dirs = ["target"]     # exact directories below the project root
```

Some checks write a huge or constantly changing build directory: `cargo test`
fills `target/`. awa watches that directory as generated state, so a run either
cannot observe it (`effect-state-unavailable`) or sees it change
(`effect-state-differs`), and never becomes reusable. A validation policy lets
one exact invocation replay its recorded *result* anyway.

## What a policy asserts

Declaring one states two independent things about each unobserved directory:

1. its prior contents cannot change the command's result — exit code and output;
2. the files the command writes there are not needed after a replay.

awa checks neither. If a test reads a file generated earlier, the first is false;
if a later step uses the build, the second is. Declare a policy only when both hold.

## Exact selection

- A policy applies only when the complete child argv equals `argv` element for
  element and the execution directory equals `cwd`, both relative to the project
  root. Without `--cwd` that is the directory you invoked awa from, so `cwd = "."`
  matches only a run started at the root. Nothing is joined, prefixed, globbed,
  or aliased: `cargo test --release` and `cargo t` stay ordinary runs. Two
  policies selecting one invocation are a configuration error.
- Each `unobserved_dirs` entry is an exact path below the project root, never a
  name matched at any depth and never a pattern: `target` is the root's `target/`,
  not `crates/x/target/`, and `*` is a literal character. The root itself, an
  absolute or `..` path, and `.git`/`.awa` are refused.
- Only a real directory at that path is waived, also when the path passes
  through a symlink awa already follows; a file or symlink at the path itself is
  observed as usual, and the same directory under another name is not waived.
- There is no flag: `awa run` and `awa run explain` select the policy themselves.
  `awa config effective` lists the policies in effect and the layer each came from.

## Conflicting scope

A `--scope` or `--include` operand naming a waived directory, or a path inside
one, asks awa to observe what the policy says not to: the run is refused with exit
2 before anything is scanned or executed. The same conflict from
`run.default_scope` is a configuration error, exit 4. A broader scope is fine —
`.`, or `artifacts` with `artifacts/cache` waived — the policy removes the waived
subtree from it.

## What changes for the matched run

- The waived directories are not traversed for input identity or for effect
  observation, before or after the command. Their size, unreadable files,
  changes, and deletion do not block reuse.
- Everything else still applies: the other inputs and watched roots, the mutation
  guard, stdin/TTY, trust mode, skipped inputs, `--record`, `--no-cache`, and
  failed-run caching.
- The policy is part of the cache key: changing its argv, cwd, or directories
  misses. Renaming it does not. Editing `awa.toml` still misses once, because the
  file is itself a run input.
- Each stored run keeps the policy it ran under. Changing the config later never
  gives an older run a waiver it did not have.

## What a hit means

```text
  replay:   result only (policy cargo-test); not observed or recreated: target
```

The stored exit code, stdout, and stderr are replayed; nothing is written to the
waived directories, so a deleted `target/` stays deleted. A hit is not evidence
that build artifacts exist or are current, nor that a fresh build would print the
same log or take the same time. Run the build itself before a step that needs
its artifacts.

## Evidence limits

A waived directory is unobserved — never reported as unchanged or empty.
`awa changes` and `awa diff` between run states say nothing about it, and
`awa restore` from a run state deletes nothing below it: the deletion is withheld
as `ignored-boundary`, or first as `policy-incompatible` when the run's scan
policy differs from the current one.
Checkpoints and their history scope are unaffected.

Files outside the project are outside the evidence too. A policy does not make a
toolchain change — a different Rust toolchain or compiler — invalidate a stored
result; only facts awa keys, such as names in `[run].env_allowlist`, do.

## Inspect

```text
awa run explain -- cargo test     # replay: on a hit, else policy:; JSON current.validation
awa run show --last --meta        # the policy that run recorded
```

`awa run --json` reports `inputs.validation`; `run show`, `run ls`, `run log`, and
explain candidates report the policy each stored run recorded.

## See also

- [awa help run](run.md)
- [awa help config](../reference/configuration.md)
- [awa help diff](diff.md)
- [awa help restore](restore.md)
