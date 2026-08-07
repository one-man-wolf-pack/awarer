# Releasing awa

This is the checkout-dependent operator procedure for a binary release.

## Repository Setup

Release Please authenticates as a GitHub App. The repository needs:

- variable `RELEASE_PLEASE_APP_CLIENT_ID`;
- secret `RELEASE_PLEASE_APP_PRIVATE_KEY`;
- the App installed with contents and pull-request write access;
- the same App installed on `one-man-wolf-pack/homebrew-tap` with contents write;
- the active `release-tags` ruleset described below.

The release job reuses that App a second time, for the Homebrew cask only. It mints an
installation token naming `homebrew-tap` and no other repository, and that token reaches
one environment variable of the GoReleaser step. Nothing long-lived is stored for the
tap, and the release itself is still published with the workflow's own token.

Who may create a `v*` tag is decided by that ruleset. It targets tags, includes
`refs/tags/v*` with no exclusion, and restricts creation, update, and deletion; the
`one-man-wolf-pack-release-please` App is its only bypass actor, with `always` bypass. No
other actor — a collaborator, an administrator, or another App — creates, moves, or
deletes a release tag while that ruleset is active, so a hand-made tag is refused rather
than being a matter of convention.

That ruleset is the only remote rule this repository claims. `main` takes direct commits:
no branch protection, no required pull request, and no release environment or reviewer
gate. What authorizes a publication once the tag exists is in
[`.github/workflows/release.yml`](../.github/workflows/release.yml): the tag must be
strict `vMAJOR.MINOR.PATCH`, must name the commit that was checked out, that commit must
be an ancestor of `main`, and `just release-gate` must pass against it.

Do not edit release assets by hand. What is built and published is
[`../.goreleaser.yaml`](../.goreleaser.yaml).

## Reviewed Development Tooling

Validation and release fetch executable dependencies beyond the Go toolchain: the task
runner every job that runs a repository command installs, the virtual machine that
supplies the one shipped platform GitHub does not host, the release packager and its
action, and the four analyzers the quality gates run through `go run module@version`.

This table is the review record: a human read the selected commit and licence and dated
it. Nothing regenerates or reconciles it, so a pin and its row move in the same change or
the record is wrong. What is enforced automatically is narrower and does not depend on
this table — every `uses:` in every workflow must name a full commit SHA, the Cloudflare
credential must stay inside the one upload step, and inside `release.yml` the App private
key must stay inside the one step that mints the tap token.

The rows are third-party actions and independently versioned executable tools. A
GitHub-maintained `actions/*` step is pinned to an exact commit like every other, but it
gets no row: its identity and licence are GitHub's, and the only thing review adds is
reading the pin where it changes, which is the workflow diff itself. So `actions/checkout`,
`actions/setup-go`, `actions/setup-node`, and `actions/create-github-app-token` are absent
here by policy, not by oversight — a row for one of them would be a record nobody
independently checks.

| Tool | Pin | Upstream | License | Reviewed |
| --- | --- | --- | --- | --- |
| `extractions/setup-just` | `53165ef7e734c5c07cb06b3c8e7b647c5aa16db3` (v4.0.0) | https://github.com/extractions/setup-just | `MIT OR Apache-2.0` | 2026-08-01 |
| `just` | `1.57.0` | https://github.com/casey/just | `CC0-1.0` | 2026-08-01 |
| `vmactions/freebsd-vm` | `77ed28d336d03fe19a3f4f7266c1d2c4714dd79d` (v1.5.2) | https://github.com/vmactions/freebsd-vm | `MIT` | 2026-08-03 |
| `goreleaser/goreleaser-action` | `f06c13b6b1a9625abc9e6e439d9c05a8f2190e94` (v7.2.3) | https://github.com/goreleaser/goreleaser-action | `MIT` | 2026-08-04 |
| `goreleaser` | `v2.17.1` | https://github.com/goreleaser/goreleaser | `MIT` | 2026-08-04 |
| `staticcheck` (`honnef.co/go/tools`) | `v0.7.0` | https://github.com/dominikh/go-tools | `MIT` | 2026-08-05 |
| `golangci-lint` (`github.com/golangci/golangci-lint/v2`) | `v2.12.2` | https://github.com/golangci/golangci-lint | `GPL-3.0` | 2026-08-05 |
| `govulncheck` (`golang.org/x/vuln`) | `v1.6.0` | https://github.com/golang/vuln | `BSD-3-Clause` | 2026-08-05 |
| `actionlint` (`github.com/rhysd/actionlint`) | `v1.7.12` | https://github.com/rhysd/actionlint | `MIT` | 2026-08-05 |

The FreeBSD action runs the `freebsd-portability` lane's focused suite in a FreeBSD
15.1 `x86_64` guest on an ordinary GitHub-hosted Ubuntu runner, with repository
`contents: read`, no environment and no secrets. Its VM image is part of the reviewed
versioned action path and is not separately downloaded, checksummed, or mirrored by this
repository. Inside the guest the Go toolchain comes from the platform's own package set
(`pkg install -y go126`) and must satisfy `go.mod` unaided: `GOTOOLCHAIN=local` is set
before anything runs, so a packaged Go older than the `go` directive fails the job
instead of pulling a different one.

The analyzers are pinned in the `justfile` and executed there through
`go run module@version`; the version this table records is the one that file interpolates.
`golangci-lint` is GPL-3.0, which applies to the tool itself: it is invoked as a separate
program and is linked into nothing this project distributes, so it imposes no obligation
on the shipped binary.

Every entry is a development dependency. None is linked into or distributed with `awa`,
so none appears in `third_party/licenses.json` or `THIRD_PARTY_NOTICES` — those cover the
shipped binary's production graph and nothing else. When a pin changes, review the
selected tool and license and update this table in the same change. The reviewed
Cloudflare deployment executables have their own table in
[`../site/DEPLOYMENT.md`](../site/DEPLOYMENT.md).

## Local Rehearsal

Prove the commit before merging a release pull request:

```bash
just release-gate
```

Every change to [`../.goreleaser.yaml`](../.goreleaser.yaml) carries both commands below
and an inspection of what they produced. No CI lane installs GoReleaser, so this is the
only place a configuration defect is caught before a release fails on it.

```bash
go run github.com/goreleaser/goreleaser/v2@v2.17.1 check
go run github.com/goreleaser/goreleaser/v2@v2.17.1 release --snapshot --clean
```

The snapshot writes six archives, their checksum file, and the per-target binaries
under `dist/`, which is generated only and never committed. A snapshot names itself
`<version>-SNAPSHOT-<sha>` and uploads nothing. Inspect the archive contents, the
checksum file, and `dist/awa_<host-target>/awa version` before trusting a config change.

The snapshot also writes `dist/homebrew/Casks/awarer.rb` and pushes nothing, because the
tap token is read only when GoReleaser publishes. Read it too: it must carry exactly four
host clauses — `on_macos` and `on_linux`, each with `on_intel` and `on_arm` — whose URLs
and SHA-256 values match the archives and checksum file beside it, and one `binary "awa"`.
The freebsd and windows archives have no clause; GoReleaser drops them.

## Cutting A Release

1. Land Conventional Commits on `main`.
2. Review and merge the release pull request maintained by Release Please.
3. Let Release Please create the version tag and GitHub Release.
4. Watch the **Release** workflow's `release` job: it re-proves the tag, runs the
   release gate, and attaches the assets.
5. Confirm the completed Release has one archive per target and its checksum file.
6. Confirm the same job updated `Casks/awarer.rb` in `one-man-wolf-pack/homebrew-tap` and
   that its version and checksums are this release's.

Release notes are generated from Conventional Commit subjects. There is no
separate hand-maintained release-notes document.

## Workflow Map

- [`.github/workflows/validate.yml`](../.github/workflows/validate.yml) runs each
  validation lane once per event and adds the push-only stress, built-binary smoke, and
  FreeBSD runtime evidence on `main`.
- [`.github/workflows/release-please.yml`](../.github/workflows/release-please.yml)
  maintains the release pull request and creates the tag and Release.
- [`.github/workflows/release.yml`](../.github/workflows/release.yml) proves the tag,
  runs the release gate, and has GoReleaser attach the archives and checksums and update
  the Homebrew cask; a second best-effort job then publishes that release's documentation
  site.

A failed release is loud and stays failed. Read the first failing named step. If assets
were partially attached, inspect and remove them before re-running the workflow —
nothing classifies, adopts, or overwrites existing release state.

A failed cask update fails the same job, after the archives are already uploaded. Those
assets stay valid and authoritative: repair the named permission or configuration failure
and follow the procedure above. Nothing reconciles the tap or rolls a release back, and
the tap is never a second source of truth for what a release contains.

Site publication happens automatically once a release is complete. Its setup,
failure diagnosis, and rollback are in
[`../site/DEPLOYMENT.md`](../site/DEPLOYMENT.md).
