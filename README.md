# awa

`awa` is a local worktree checkpoint, comparison, supervised-command, and run
evidence tool for humans and coding agents. It is a single Go binary with no
daemon, server, telemetry, or runtime dependency. Project-local state lives
under `.awa/`.

This repository is the complete source for `awa`. It builds the binary, its
embedded operational documentation, the release artifacts, and the small public
documentation site.

## Requirements

- Go 1.26 or newer; `go.mod` owns the exact toolchain floor.
- Git for repository workflows and release tooling.
- Just 1.57 or newer for the repository task surface; every individual operation
  also runs as a plain Go command, so Just is a convenience, never a dependency of
  building, testing, or using `awa`.
- Lefthook 2.1 is optional, for local commit-message feedback only. It is not
  installed automatically and nothing in the build, test, review, or release path
  depends on it.
- No runtime dependency for a released `awa` binary.

Pinned analysis tools run through `go run`; a separate system installation is
not required.

## Install

Release archives and their SHA-256 checksum file are published on the
[GitHub Releases page](https://github.com/one-man-wolf-pack/awarer/releases).
Download the archive for your platform, verify it against
`awa_<version>_checksums.txt` — `<version>` is the release tag without its leading
`v` — extract the binary, and place it on `PATH`.

```bash
sha256sum --check --ignore-missing awa_1.2.3_checksums.txt       # Linux
shasum -a 256 --check --ignore-missing awa_1.2.3_checksums.txt   # macOS
awa version
```

The installed binary carries the complete installation and upgrade guidance:

```bash
awa help install
```

To build from source:

```bash
go build -o awa ./cmd/awa
./awa version
```

## First Use

```bash
awa init
awa status
awa checkpoint -m "before the change"
# edit the worktree
awa changes
awa diff
awa run -- go test ./...
```

## Recovering From an Unwanted Rewrite

When a build, generator, or formatter rewrites files it should not have, restore
just those paths from the checkpoint you were working against. Preview is the
default and changes nothing; `--apply` is the only mutating form:

```bash
awa restore <checkpoint-id> -- generated/client          # preview
awa restore --apply <checkpoint-id> -- generated/client  # perform it
```

Restore only writes what one stored state proves, and only where the current path
still matches what the preview described: nothing outside the selection is
touched, incomplete evidence refuses with a named reason instead of overwriting,
and there is no force option. Ignored paths are outside awa's evidence and are
never restored or deleted. Every apply first records the state it overwrote and
prints a `restore:<id>:before` reference to undo it, retained under
`[gc].keep_restores_for`. See `awa help restore` for the full boundary.

Start with `awa help agents` when using the tool from an agent workflow. The
binary, rather than this README, owns current commands, configuration, safety
boundaries, JSON shapes, and exit behavior:

```bash
awa help agents
awa help topics
awa help quickstart
awa help config
awa help restore
awa docs export --output <directory>
```

`awa docs export` emits the same deterministic documentation corpus used by the
public site. Exact CLI metadata is generated from the live registries and is
checked against [`internal/cli/testdata/reference.json`](internal/cli/testdata/reference.json).

## Development

Recurring workflows run through [`just`](https://just.systems) 1.57 or newer:

```bash
just              # list the public task surface
just check        # the fast gate: format, vet, test, race, lint, reference, licences
just test         # Go test suite
just stress       # stress-tagged acceptance scenarios
just site         # build the public site into site/dist from a fresh export
just release-gate # complete pre-release gate
```

`just --list` is exhaustive for the public task facade, not for everything this
repository can do. Focused one-step work keeps its native owner and is better run
directly:

```bash
go build ./...                             # build
gofmt -w .                                 # format
go test -run '^$' -bench . -benchmem ./... # benchmarks
go run ./internal/tools/refgen             # emit the CLI reference
awa docs export --output <directory>       # export the documentation bundle
```

CI invokes the same recipes, so a locally green gate is green for the same reason.
Agent workflow instructions are in [`AGENTS.md`](AGENTS.md). To report a defect
or propose a change, see [`CONTRIBUTING.md`](CONTRIBUTING.md).

### Commit messages

A commit that declares no co-author passes. Once it declares one, Git's final
trailer block must hold exactly these two lines, once each and in any order:

```text
Co-authored-by: Codex <noreply@openai.com>
Co-authored-by: Claude <noreply@anthropic.com>
```

Enforcement is the Validate workflow, which applies the check to every commit a
push or pull request introduces. It runs once, in the `check` lane.

```bash
./scripts/check-co-authors.sh <file> # try it against a message
lefthook install                     # opt in to local feedback
lefthook uninstall                   # opt back out
```

The optional `commit-msg` hook runs the same check and only makes the answer
arrive sooner; skipping it or never installing it cannot change what the remote
check decides. Use the script directly rather than `lefthook run` to test a
message: `run` synchronizes the hook into `.git/hooks` as a side effect.

## Repository Map

- [`cmd/awa`](cmd/awa) — binary entry point.
- [`internal/cli`](internal/cli) — command routing and human/machine projection.
- [`internal/app`](internal/app) — application use cases.
- [`internal/domain`](internal/domain) — domain values and invariants.
- [`internal/infra`](internal/infra) — filesystem, process, storage, SQLite, and
  Git adapters.
- [`test/acceptance`](test/acceptance) — built-binary acceptance scenarios.
- [`site`](site) — static-site generator, local server, and site documentation.
- [`internal/tools`](internal/tools) — repository-local generators, audits, and
  release tools.
- [`scripts`](scripts) — repository shell checks the hook and CI call directly.
- [`.github/workflows`](.github/workflows) — CI, release, and site orchestration.

Runtime entry points other than `cmd/awa` are repository tools, not additional
product commands: `site/sitegen`, `site/siteserve`, and the programs under
`internal/tools` are invoked through the [`justfile`](justfile) or directly with
`go run`.

## Focused Documentation

- [`site/README.md`](site/README.md) — building and inspecting the static site.
- [`.goreleaser.yaml`](.goreleaser.yaml) — the release targets, archive contents,
  names, checksums, and generated Homebrew cask GoReleaser produces.
- [`THIRD_PARTY_NOTICES`](THIRD_PARTY_NOTICES) and
  [`third_party/licenses.json`](third_party/licenses.json) — reviewed third-party
  legal material and its machine-checked inventory.

## License

`awa` is licensed under the [Apache License 2.0](LICENSE).
