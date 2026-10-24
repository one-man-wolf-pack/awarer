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

Files outside the project are outside the evidence too. A policy alone does not
make a toolchain change invalidate a stored result; only facts awa keys do, such
as names in `[run].env_allowlist` or the identity probes below.

## Identity probes

```toml
[[run.validation.identity_probes]]   # in awa.toml, inside the policy above
name = "rustc"                       # a label for output only
argv = ["rustc", "-vV"]              # a literal command, never a shell line
```

```toml
[run]                                # in .awa/config.toml only
allow_identity_probes = true
```

A probe is a command whose exact stdout becomes part of the policy's cache key,
so a different report means a miss. Up to 8 probes per policy.

- **Permission.** Probes are code from the project config. They run only when
  `allow_identity_probes = true` is set in the private `.awa/config.toml`; in
  `awa.toml` or a `--config` file that key is a config error. The grant covers
  every probe this checkout's config declares, including ones added or edited
  later. It is not a sandbox: a probe can do anything a command can.
- **When.** Only `awa run`, never explain, `run ls`, show, status, doctor, or
  config. A run that could be reused runs the probes before it looks up a result,
  also when that becomes a hit, and again after the command before it stores a
  reusable result. `--record`, `--no-cache`, disabled capture, unkeyed stdin, and fast trust mode
  skip them. Each runs in the command's cwd with the command's sanitized
  environment and null stdin.
- **Bounds.** Each probe gets 10 s, 64 KiB of stdout, and 64 KiB of stderr; one
  phase of probes gets 30 s. Exceeding a bound ends that probe.
- **Unavailable.** A probe that is not granted, is not found, fails to start,
  exits non-zero or by a signal, times out, prints nothing, or overflows a bound
  makes the facts unavailable. The command still runs with its own exit code and
  output; only reuse is lost. Before the run that is `external-facts-unavailable`;
  after it, `external-facts-unavailable-post-run`. Different bytes after the run
  are `external-facts-changed`. Without the grant, explain, `run ls`, and status
  report that same blocker with every probe `denied`, and run none.
- **Privacy.** Probe output is never stored or shown, also on failure. Runs keep
  each probe's status and a fingerprint of its stdout. A fingerprint is not
  encryption: a short guessable report can be guessed.

Matching reports prove only that the probes printed the same bytes at those two
moments. They do not identify libraries, compiler components, build-script
discoveries, or services the probes do not report. awa does not check that a
probe describes the tool the command uses: for `cargo +nightly test`, plain
`rustc -vV` can report a different toolchain, so name the same selection in the
probe argv.

## Inspect

```text
awa run explain -- cargo test     # replay: on a hit, else policy:; JSON current.validation
awa run show --last --meta        # the policy and facts that run recorded
```

`awa run --json` reports `inputs.validation`; `run show`, `run ls`, `run log`, and
explain candidates report the policy each stored run recorded. With identity
probes, `identity_probes` lists each probe's `before` and `after` status and
fingerprint.

## See also

- [awa help run](run.md)
- [awa help config](../reference/configuration.md)
- [awa help diff](diff.md)
- [awa help restore](restore.md)
