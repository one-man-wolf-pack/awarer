# awa local evidence and privacy

awa keeps durable local evidence under `.awa/` so you can review, replay, and
audit work. That evidence is useful, but it means `.awa/` must stay private and
untracked.

## What `.awa/` stores

- checkpoint manifests and, when `store_file_contents` is on (default), the
  file contents themselves as content-addressed blobs;
- run cache/history: the command line, cwd, exit status, timings, and captured
  stdout/stderr;
- the run key inputs and diagnostics.

Three facts combine in a way worth stating explicitly: a checkpoint always covers
untracked files, checkpoints store file contents by default, and `.gitignore` is
not an input boundary (see [awa help ignores](ignores.md)). So a gitignored,
never-committed file — a local `.env`, a key, a seeded fixture database — is
inside the scanned scope and its bytes can be stored as a checkpoint blob.
Deleting the file from the worktree later does not remove it from an older
checkpoint. If that is not what you want for a particular path, exclude it
deliberately rather than relying on git having ignored it.

## What it does NOT store

- raw environment values. Allowlisted env values are keyed and passed to the
  child, but the durable record keeps only a redacted identity (presence plus a
  value fingerprint), never the raw value — so a secret in an allowlisted
  variable is not written to disk.

## The environment a wrapped command runs with

`awa run` does not hand the child your whole environment. It builds one: a built-in
baseline, plus whatever `[run].env_allowlist` adds, plus a fixed fact awa injects —
and nothing else. Refusing full inheritance is what makes the cache honest. Every
value the child can see is in the cache key, so no variable can change a result
behind awa's back, and no unrelated secret in your shell reaches a wrapped command.

The baseline is what a command needs in order to run, and what decides how it reads
and writes text:

```text
PATH HOME USER LOGNAME SHELL TMPDIR TEMP TMP    execution
SystemRoot WINDIR COMSPEC PATHEXT               execution (Windows)
LANG LANGUAGE LC_ALL LC_COLLATE LC_CTYPE        locale
LC_MESSAGES LC_MONETARY LC_NUMERIC LC_TIME      locale
```

Locale is inherited exactly as you have it. awa passes the state it found —
unset stays unset, empty stays empty, a value is passed byte for byte — and never
forces UTF-8, never falls back to `C`, and never invents a value you did not set.
That matters because locale is not a preference: a reader that decodes UTF-8 under
your locale decodes US-ASCII without it. Each locale variable is keyed like any
other, so changing one is an honest cache miss rather than a silently different run.

Everything else is opt-in, by name, in `[run].env_allowlist`. The shipped default
is `["CI", "NODE_ENV"]`; add what your commands actually depend on:

```toml
[run]
env_allowlist = ["PYTHONPATH", "MY_TOOL_MODE"]
```

Some families are deliberately never in the baseline, because inheriting them by
default would be wrong for most projects:

```text
tokens, keys, passwords, SSH_AUTH_SOCK, cloud   credentials and capabilities
LD_PRELOAD, DYLD_*, BASH_ENV, NODE_OPTIONS      loads or runs extra code
GOFLAGS, CARGO_HOME, JAVA_HOME, PYTHONPATH      toolchain settings
XDG_CONFIG_HOME, XDG_CACHE_HOME, XDG_DATA_HOME  external config and cache roots
http_proxy, SSL_CERT_FILE, npm registry vars    network trust
EDITOR, PAGER, TERM, COLUMNS                    interactive shaping
```

You can allowlist any of them when a command genuinely needs it — that is a
deliberate decision, and `awa doctor` will say so for the names that carry secrets
or that can load code. A path-valued setting keeps one limit either way: awa keys
the value, not what lives behind it, so the contents of those directories stay
outside the evidence boundary and a cache hit does not prove they are unchanged.
`PYTHONPATH` is the worked example, and why it is not a default: awa can tell that
the path string is the same, never that the importable code behind it still is.

## The `AWA_RUN` marker

Every child awa actually executes receives exactly:

```text
AWA_RUN=1
```

It lets a cooperative tool notice it is running under the wrapper — to pick quieter
output, say. It is advisory and nothing more. Any process can set it, so it proves
nothing: not that awa is present, not that a run record exists, not that anything is
supervised. Never treat it as authentication or as permission to skip a check. It is
fixed on purpose: no run id, timestamp, or store path travels through the
environment. It is reserved, so `env_allowlist` rejects the name, and a cache hit
executes no child and injects nothing.

Treat captured output as evidence, not as sanitized text: stdout/stderr are
stored verbatim, so a command that prints a secret stores that secret. awa does
not redact command output (that would hide real compiler/test evidence). If
output exceeds the capture limit it is stored truncated and every surface says
so ("stored ... is incomplete evidence — N bytes omitted"); a truncated payload
is never presented as complete.

## Keep `.awa/` private and untracked

- awa creates `.awa/` and everything in it owner-private; do not widen it.
- `.awa/.gitignore` keeps the directory out of git; awa restores it on
  checkpoint and run, and `awa doctor` reports if it is missing or ineffective.
  awa never edits your repository-root `.gitignore`.
- `awa doctor` also flags secret-looking env allowlist names, content storage in
  a project that appears to hold secrets, overly broad permissions, and nested
  or ancestor `.awa` markers.

Before you copy, snapshot, or share a repository, remember `.awa/` travels with
it:

```text
awa doctor              # check guard, permissions, and root hygiene
awa gc                  # reclaim unreferenced evidence
rm -rf .awa             # or delete it entirely if you do not want the evidence
```

Deleting the directory also removes the private `.awa/config.toml` layer, which
lives inside it; a committed `awa.toml` and `.awaignore` are outside `.awa/` and
survive. Note too that `awa gc` is a retention tool, not a sanitizer: it reclaims
only what no retained checkpoint or run still references, and by default keeps
the latest checkpoint and the configured keep-last window. If the goal is that
nothing leaves with the copy, delete the directory or ship a fresh `git clone`
rather than a directory copy.

## Restore recovery observations

An applied `awa restore` records the worktree state it overwrote — including file
content — under `.awa/restores/`, so the restore can be undone. That means it is
evidence of the same kind as a checkpoint's blobs: private, owner-only, and never
to be committed. It is retained under `[gc].keep_restores_for` (default `14d`),
so ordinary `awa gc` reclaims it in time; `awa gc --older-than` reclaims it
sooner. See [awa help restore](restore.md).

## See also

- [awa help doctor](doctor.md)
- [awa help run](run.md)
- [awa help inspect](inspect.md)
- [awa help ignores](ignores.md)
- [awa help gc](gc.md)
