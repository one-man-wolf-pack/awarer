# awa doctor — diagnose and repair local state

```text
awa doctor [--repair] [--strict]
```

`awa doctor` inspects the durable `.awa/` state — the checkpoint store, blob
store, run cache, worktree index, lock files, and the `.awa/` git guard — and
prints a one-line health verdict plus one line per finding. It also
raises local evidence/privacy findings: secret-looking env allowlist names,
content storage in a project that appears to hold secrets, overly broad `.awa/`
permissions, and nested or ancestor `.awa` markers (see
[awa help privacy](privacy.md)).

```text
awa doctor              # report health and any findings
awa doctor --repair     # also perform the safe, mechanical repairs
awa doctor --strict     # recompute hashes instead of trusting stat signatures
awa doctor --json       # machine-readable findings (severity, subsystem, code)
```

Each finding is tagged by the action it invites: `[repaired]` for one `--repair`
fixed, `[repairable]` for one `--repair` would fix, otherwise its severity.
`--repair` performs only safe, mechanical fixes (such as restoring the `.awa/`
git guard or removing an orphaned temp artifact); it never touches your worktree.

The exit status is the diagnosis, not an awa failure (like `awa run`, there is
no "awa:" error line): a healthy or warnings-only run exits 0, while unrepaired
errors or unrepaired repairable findings exit 5 (local state needs an action).
That code does not mean damage on its own: an unrepaired guard file earns it just
as a corrupt record does, so read the findings rather than the code.

## Finding codes

Every finding carries a stable machine code from a closed set, so a script can
branch on the code rather than on the message. They group by subsystem:

```text
config-invalid                a config layer is present but not valid
required-dir-*                a required .awa/ directory is missing or wrong
checkpoint-*                  a checkpoint record, manifest, or blob is corrupt,
                              missing, or in a schema this awa cannot read
run-*                         a stored run's metadata, pointer, or payload is
                              corrupt, or in a schema this awa cannot read
restore-recovery-*            a restore recovery observation is unreadable, or
                              its captured content is gone — that restore can no
                              longer be undone
index-*                       the worktree index is unreadable, schema-invalid,
                              or stale
lock-stale, lock-unknown      a lock file left behind, or one awa cannot classify
orphan-temp, temp-unreadable  a leftover temp artifact under .awa/, or a temp
                              location awa cannot read
state-gitignore-*             the awa-owned .awa/ git guard is missing,
                              ineffective, or unreadable
awa-tracked-by-git            .awa/ content is tracked by git — evidence is being
                              committed
git-check-failed              that git check could not be run at all
state-permissions-too-broad   .awa/ is readable beyond its owner
env-allowlist-suspicious      an allowlisted env name looks like a secret
env-allowlist-injects-code    an allowlisted env name can load or execute code
                              in the wrapped command (LD_PRELOAD, DYLD_*,
                              BASH_ENV, NODE_OPTIONS, RUBYOPT, ...)
content-storage-enabled       file contents are stored in a project that looks
                              like it holds secrets
nested-*, ancestor-*          another .awa marker below or above this root, or a
                              nested-marker scan that could not complete
repair-failed                 a repair was attempted and did not succeed
```

## What --repair will and will not do

`--repair` performs exactly four safe, mechanical, awa-owned fixes, each for the
findings that name it:

- restores the awa-owned `.awa/.gitignore` guard when it is missing or
  ineffective;
- removes an orphaned temp artifact under a known `.awa` temp location;
- removes a lock record it proved stale — never one it cannot classify;
- invalidates the worktree index when it is unreadable or stale, so the next scan
  rebuilds it. The index is acceleration-only state, so dropping it loses nothing
  durable.

Everything else is diagnosed and left alone, including two cases worth knowing
about before you reach for `--repair` expecting them. Overly broad `.awa/`
permissions are reported, not changed: the finding names the exact `chmod` to
run, because changing your filesystem permissions is your decision. And
reclaiming unreferenced records is [awa help gc](gc.md)'s job — `doctor` never
deletes a checkpoint, run, or blob.

It never touches your worktree, never deletes a checkpoint or run you might
still want, and never rewrites a corrupt record into a plausible-looking one. A
finding it cannot fix safely stays reported — but only the findings the four
repairs above cover are counted as repairable, so a finding doctor will never fix
does not hold the exit code at 5 forever.

## When to use --strict

`--strict` is not a doctor-local flag: it is the shorthand for the global
`--trust-mode strict`, which doctor honors. That is why it appears in the usage
line but not in the per-command flag table.

By default the checks trust stat signatures (size and modification time) where
they can. `--strict` recomputes content hashes instead. It is slower — on a large
store, substantially — and it is the right choice when you suspect a file changed
without its metadata changing, after a crash, or after copying a project between
machines or filesystems.

`--repair` and `--strict` combine: `awa doctor --repair --strict` diagnoses by
content and then applies the safe fixes that diagnosis found.

## See also

- [awa help gc](gc.md)
- [awa help troubleshooting](troubleshooting.md)
- [awa help privacy](privacy.md)
- [awa help exit-codes](exit-codes.md)
