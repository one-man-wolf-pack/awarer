# awa platform and filesystem support

awa is a local, single-user tool. Its correctness assumptions are those of a
local filesystem; behavior elsewhere is best-effort unless stated otherwise.

## Platforms

A release publishes six targets, and they do not all carry the same
evidence. The distinction is deliberate: "we build it" and "we run it" are
different claims, and the two do not line up the way the target list suggests.

```text
linux/amd64      Linux — the CI runtime platform: the whole test suite and race
                 detector on every change, plus the stress suite on each push to
                 the default branch
darwin/arm64     macOS — the development platform, exercised interactively every
                 day, but not covered by CI
windows/amd64    Windows — compile-only, plus a narrow CI runtime suite for the
                 filesystem primitives, the worktree mutation restore writes
                 through (including symbolic links, which that lane requires
                 rather than skips), and the documentation export, where its
                 semantics differ enough to decide correctness
freebsd/amd64    FreeBSD — compile-only, plus a focused CI runtime suite in a
                 FreeBSD 15.1 (x86_64) virtual machine on each push to the
                 default branch, covering the filesystem primitives, the private
                 local state and stores built on them, the collector lock, the
                 scanner, the worktree mutation restore writes through (symbolic
                 links required, not skipped), doctor, and the documentation
                 export; that run must be green before the release is authorized
darwin/amd64     compile-only
linux/arm64      compile-only
```

Compile-only means the binary is built for that platform in CI but is not
exercised at runtime there, so its behavior is best-effort. Treat a failure on
any published target as a bug worth reporting, not as an unsupported
configuration.

Windows and FreeBSD both carry a focused runtime suite, but they do not carry the
same local-filesystem guarantees. FreeBSD runs the same native implementations as
Linux and macOS: descriptor-relative no-follow operations, so an operation reaches
its target through the directory it opened and a path swapped underneath it cannot
redirect the write; a kernel `flock` for the collector lock, so a crashed holder
releases it without anything to detect or steal; and complete change-time, device,
inode, and link-count fields in a scan signature. Windows exposes none of those,
and its lane proves the weaker behavior it actually runs — the final path component
is checked before the open rather than as part of it, an already-open directory is
re-resolved by name, the exclusive lock is a best-effort file-content protocol, and
those four stat fields are recorded as unavailable rather than guessed.

Two consequences worth naming, because the ordering above is not the intuitive
one: macOS is the most-used platform but the least automatically verified, and
Linux is the most verified even though it is not the one this project is
developed on.

## Filesystem assumptions

- awa relies on local-filesystem semantics: flock advisory locks, atomic rename
  and hard-link publication, and owner-private permissions.
- Network filesystems (NFS, SMB) are best-effort. Their locking and
  atomic-rename guarantees can differ, so concurrent use across hosts is not
  supported.

## Paths, case, and links

- paths in manifests and JSON are stored relative to the project root with `/`
  separators, on every platform.
- case is preserved exactly in manifests and JSON. Manifests assume a
  case-sensitive filesystem; on a case-insensitive one, two paths that differ
  only by case map to a single file — awa does not detect that, so treat
  case-insensitive filesystems as best-effort.
- symlinks and hardlinks follow the scanner/checkpoint policy: symlinks are not
  followed by default (see `follow_symlinks` / `symlink_max_depth`), and a
  hardlink is treated as a regular file for content identity.
- `.awa/` is assumed to be local, private state on the same filesystem as the
  project.

## Environment names and locale across platforms

- environment variable names are case-sensitive on Unix and case-insensitive on
  Windows. `[run].env_allowlist` therefore rejects names that differ only by case
  on every platform, so one committed `awa.toml` behaves the same everywhere.
- the POSIX locale variables (`LANG`, `LC_*`, `LANGUAGE`) are in the built-in
  baseline on every platform, including Windows, because being in the baseline
  means "inherit whatever the parent has". A Windows process usually has none of
  them, so nothing is passed and each is keyed as unset; a Windows process that
  does define one — under a POSIX-flavored shell or a cross-platform toolchain —
  has it passed through unchanged. awa never invents a POSIX locale value on a
  platform that does not use one.

## See also

- [awa help install](install.md)
- [awa help privacy](privacy.md)
- [awa help doctor](doctor.md)
- [awa help troubleshooting](troubleshooting.md)
