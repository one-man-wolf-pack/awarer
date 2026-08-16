# installing, verifying, and upgrading awa

`awa` is one static binary. It has no runtime dependencies, no daemon, no
background service, no network calls, and no telemetry. Installing it means
putting one file on `PATH`; removing it means deleting that file.

A release reaches you two ways: a maintained Homebrew formula on macOS and
Linux, or a release archive on every supported platform. Both give you the same
release — Homebrew compiles it from the tagged source, the archive is the
prebuilt binary — and both report the same version. Building from source
yourself is documented further down and produces a development build rather
than a release.

## Which version am I running

The installed binary knows exactly which version it is. Ask it, not a website:

```text
awa version
awa version --json
```

The human line reads `awa <version> (<short-commit>, <go-toolchain>)`. A build
that was not stamped by the release tooling reports the version `0.0.0-dev`;
treat that as "built from source", not as a release.

The JSON form carries the same facts in the standard envelope described in
[awa help json](json.md): `version` and `go` always, plus `revision` — the full
commit, not the shortened one — and `time` when the Go toolchain stamped VCS
metadata into the build. A release binary is built from a checkout and carries
both; one built from an extracted source tree has no VCS to read, so it reports
neither and its human line shows no commit in the parentheses.

`awa version` takes no global options other than `--json`: it never reads a
project, a config file, or `.awa/`, so it answers the same way anywhere.

## Install with Homebrew

The formula is `awarer`; it installs `awa`. Use the fully qualified name because
the formula is in a third-party tap and Homebrew already assigns `awa` to another
application.

```text
brew install one-man-wolf-pack/tap/awarer
awa version
```

Homebrew builds `awa` from the released source. It installs Go itself as a build
dependency, so you do not install Go first, and it is a build dependency only —
the finished `awa` is still one static binary that depends on nothing at runtime.
The trade is time: compiling takes longer than downloading an archive. What you
get back is that the binary is built on your machine rather than downloaded, so
there is nothing for macOS to mark as quarantined and no `xattr` step belongs to
this path.

Upgrading is Homebrew's ordinary flow — refresh the tap, then upgrade by name:

```text
brew update
brew upgrade awarer
```

Removing it is the counterpart:

```text
brew uninstall awarer
```

Homebrew compiles the same immutable release tag and commit the archives below
are built from, and the binary it produces reports that same version. It is not
byte-identical to an archive, because it was compiled on your machine: the
checksummed GitHub Release archives stay the authority for which versions exist
and what prebuilt bytes they contain. Homebrew covers macOS and Linux — use the
archive on Windows and FreeBSD.

## Install from a release archive

Each tagged release publishes one archive per supported target and one SHA-256
checksum file, attached as assets to that tag's release:

```text
https://github.com/one-man-wolf-pack/awarer
https://github.com/one-man-wolf-pack/awarer/releases
```

The first is the source repository, the second is where the release assets live
and is the authority for the archives described below.

Archives are named
`awa_<version>_<goos>_<goarch>.tar.gz`, except Windows, which uses
`awa_<version>_windows_amd64.zip`. In an asset name `<version>` is the release
tag with its leading `v` removed, so tag `v1.2.3` publishes
`awa_1.2.3_linux_amd64.tar.gz` — while the binary inside reports the tag as it
is, `awa v1.2.3`. `<goos>` and `<goarch>` are the Go platform names —
`linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`, `windows/amd64`,
and `freebsd/amd64` are the supported set. On macOS, `uname -m` reports `arm64`
for Apple silicon and `x86_64` for `amd64`.

Download the archive for your platform together with the checksum file
`awa_<version>_checksums.txt`, verify, then extract:

```text
sha256sum --check --ignore-missing awa_1.2.3_checksums.txt       # Linux
shasum -a 256 --check --ignore-missing awa_1.2.3_checksums.txt   # macOS
```

`--ignore-missing` is needed because the checksum file covers every target's
archive and you downloaded one. On Windows, compare the digest yourself with
PowerShell `Get-FileHash <file> -Algorithm SHA256` or
`certutil -hashfile <file> SHA256` against the matching line in that file.

Each archive contains exactly the `awa` binary (`awa.exe` on Windows),
`README.md`, `LICENSE`, and `THIRD_PARTY_NOTICES`. Move the binary somewhere on
`PATH` and confirm it runs:

```text
awa version
```

A copy downloaded through a browser on macOS carries a quarantine attribute, and
Gatekeeper refuses to run it until that attribute is cleared:

```text
xattr -d com.apple.quarantine <path-to-awa>
```

On Windows, SmartScreen may warn on first run. Clearing either is you deciding to
trust the copy you just verified — the checksum did not decide it for you, so do
it after the checksum matched, not before.

The third-party components linked into the binary are inventoried in the
`THIRD_PARTY_NOTICES` file inside every archive.

## What the checksum does and does not prove

The checksum file is an integrity manifest. It proves that the bytes you
downloaded are the bytes that were published. It is not a publisher identity: a
checksum file can be recomputed over any set of files by whoever writes them, so
it cannot certify who produced the release or that the set is the intended one.

Signing and attestation are deliberately out of scope for now. The current
provenance is the checksum file plus the binary's own version and commit
stamping.

## Build from source

```text
go build -o awa ./cmd/awa
```

Building needs Go 1.26 or newer — the exact floor is the `go` line in `go.mod`,
which the toolchain enforces for you. The produced binary needs nothing. The
module path is local, so there is no `go install <import-path>` form, and a
source-built binary reports `0.0.0-dev`. Homebrew compiles the same source but
stamps the release tag it selected, which is why what it installs reports a
release version and this does not.

## Upgrade

`awa` keeps no global state, no user-level configuration directory, and no
registry: upgrading is replacing the binary. Nothing has to be migrated before
or after.

Project state is per project and is not touched by installing a new binary: your
worktree, `awa.toml`, and `.awaignore` are files a new binary simply reads.

Existing evidence is a different question, and nothing is migrated. A record
declaring a schema this awa cannot read is not upgraded in place and is never
reusable; it is diagnosed rather than silently reinterpreted, and it is kept:

- `awa doctor` reports it under its own finding, so an unreadable schema is
  diagnosed separately from corruption, and it neither migrates nor quarantines
  the record;
- `awa gc` will not reclaim it. Nothing can prove what a record awa cannot decode
  references, so deleting it is not a safe automatic act: it is retained, the
  blob sweep stands down, and `gc` exits asking for a decision.

Clearing it is therefore always deliberate. Remove a single stored run with
`awa run rm <id>`; there is no command that deletes one checkpoint. The recovery
awa itself recommends is a fresh store.

Reset it like this, in this order:

1. Stop anything still using the project — a running `awa run`, a collector, an
   agent shell. The reset removes `.awa/locks`, and deleting a live process's
   lock is how you get a half-recreated store rather than a clean one.
2. Change to the project root. Every path below is relative to it, so running
   them from a subdirectory quietly deletes nothing, or the wrong project.
3. Delete the evidence and runtime directories:

```text
.awa/checkpoints  .awa/runs  .awa/restores  .awa/store  .awa/indexes
.awa/locks        .awa/logs
```

4. Run `awa init`.

Delete those directories, not `.awa/` itself. The state directory also holds your
private `.awa/config.toml` layer and the awa-owned `.awa/.gitignore` guard, and
removing the parent takes both with it. Resetting this way loses the local
history and leaves your worktree, git history, committed `awa.toml`,
`.awaignore`, and local config untouched. (Deleting `.awa/` outright is still the
right move when you want the evidence *and* the private config gone — see
[awa help privacy](privacy.md).)

So plan an upgrade as "the binary changes, the evidence may need cleaning up", not
as "everything keeps working". After an upgrade, or after a repository was moved,
copied, or restored from a backup, run:

```text
awa doctor
```

Downgrading is the same operation in reverse, with one caveat: a newer store may
contain records an older binary reports as unreadable rather than migrating them
backwards.

## See also

- [awa help quickstart](quickstart.md)
- [awa help platform](platform.md)
- [awa help doctor](doctor.md)
- [awa help json](json.md)
