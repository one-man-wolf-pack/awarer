# awa quick start — first project and first loop

You have the binary and a working directory. This page takes you from nothing to
a first review loop. Nothing here requires a git repository, a network, or a
configuration file.

## Initialize the project

```text
awa init
awa init --profile strict
awa init --root <path>
```

`<path>` is the directory to initialize; without `--root`, `awa init` uses the
current directory and does not walk upwards looking for a better one.

`awa init` creates a single `.awa/` directory at the project root, owner-private,
holding every durable record awa keeps. It refuses to re-initialize a directory
that already has one, so running it twice is safe and never disturbs existing
state. Initialization is all-or-nothing: a failure part-way removes the partial
`.awa/` rather than leaving something that looks complete.

The default profile writes **no configuration file at all** — the built-in
defaults apply and an absent file is the normal, correct outcome. `--profile
strict` writes one local override, `trust_mode = "strict"` under `[hashing]`.
`awa init` never prompts, so it is safe to run unattended from a script.

## The git ignore guard

`.awa/` holds local evidence, not source, and must never be committed. `awa` owns
`.awa/.gitignore` for exactly that purpose and restores it on checkpoint and run,
so state cannot quietly start accumulating unguarded.

`awa` never creates or edits your repository-root `.gitignore` — that file is
yours. The guard is the one it does own, and there is no way to opt out of it. If
it is ever missing or altered, `awa doctor --repair` puts it back.

## The first loop

```text
awa status
awa checkpoint -m "<specific-checkpoint-message>"
awa changes
awa diff
awa run -- <command>
```

`<specific-checkpoint-message>` is a placeholder: replace it with a short message
describing this particular baseline, and do not copy the placeholder literally.
`<command>` is the check you want to make reusable, for example a test or lint
invocation; everything after `--` belongs to that command, not to awa.

What each step is for:

- `awa status` is the dashboard and the single entry point before a review. It
  names the baseline, what is dirty, live git state, which runs are reusable, and
  the commands worth running next.
- `awa checkpoint` records an explicit checkpoint of the worktree and prints its
  id. Keep that id — it is how you compare against this exact moment later.
- `awa changes` summarizes what changed; `awa diff` shows the content. Both
  default to the range `latest..now`.
- `awa run` executes a command, stores its output durably, and — when the run is
  clean and non-mutating — makes it reusable for an unchanged input tree.

An empty `awa changes` means "nothing changed since that baseline". It is not
proof that the whole worktree has been reviewed; finish with the full
uncommitted surface before declaring work ready.

## Where state lives

One `.awa/` directory at the project root, and nothing else. There is no global
state, no user-level config directory, no daemon, and no network. Commands find
the root by walking up from the current directory, or use `--root <path>` to say
it explicitly. `awa` does not track files outside that root — the one way to
change that is to deliberately enable both `follow_symlinks` and
`allow_symlink_root_escape`, which are off by default.

## Configuration is optional

Defaults are built into the binary, so most projects need no configuration at
all. When you do need it, values are composed from five inputs, later ones
winning: built-in defaults, then the shared committable `awa.toml` at the project
root, then the private untracked `.awa/config.toml`, then an explicit
`--config <path>`, and finally command-line flags. A key written into the wrong
table is a config error, not a silent no-op, so check the section name in the
reference before hand-editing.

One layering rule deserves stating before you write a second layer: a later layer
replaces the whole value of a key, and for a list that means the entire list rather
than a merge. A private `.awa/config.toml` setting `extra_excludes` discards the
shared `awa.toml` list instead of adding to it — same for `include`,
`env_allowlist`, `default_scope`, and `extra_effect_roots`. To extend a shared
list, restate its entries in your layer, and use `awa config effective` to confirm
what actually resolved.

```text
awa config template
awa config init --shared
awa config init --local
awa config show
awa config effective
awa config validate
```

`awa config template` prints an annotated template to stdout and needs no
project. Use `--shared` for policy the whole team should get, `--local` for a
private override. `awa config show` prints one layer's raw file contents — name
`shared` or `local` when both exist — while `awa config effective` shows the
composed result and which layer each value came from. Reach for `show` when you
want to know what a file says, and `effective` when you want to know what awa
will actually do.

There is no command that sets a config value: `awa config init` writes a scaffold
and then you edit the TOML yourself. `awa` never edits a config file on your
behalf — in particular it will not widen or narrow the scan to improve the cache
hit rate, because that is a policy decision with consequences you should choose
knowingly.

The full key schema lives in the configuration reference:
[awa help config](../reference/configuration.md).

## See also

- [awa help agents](agents.md)
- [awa help status](status.md)
- [awa help workflows](workflows.md)
- [awa help ignores](ignores.md)
- [awa help privacy](privacy.md)
