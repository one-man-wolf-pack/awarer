# awa exit codes

awa returns stable process exit codes so scripts and agents can branch on them:

```text
0    success
1    generic error, or differences found / validation failed
2    usage error (unknown command, unknown flag, bad flag value)
3    a requested thing does not exist: the project root, a config file, or
     something the invocation named — an unmatched id prefix, an out-of-range
     @-N, an unknown run or restore id, or an observation awa does not have
4    configuration present but invalid
5    awa's local state needs an action (corruption, or an unrepaired finding)
6    a required lock could not be acquired in time
130  interrupted (Ctrl+C / SIGINT) before authoritative state was published
```

The awa-owned range is 0-6, plus 130 for interruption; every code is produced by
a real command path.

## What to do about each

```text
1    read the message: a comparison found differences, or something failed
2    fix the invocation; the message names the flag or argument
3    read the message: it names what was not found. For a project, run
     'awa init' or point at it with --root <path>; for something the
     invocation named, check it exists — 'awa log -n 5' for checkpoints,
     'awa run ls' for runs
4    run 'awa config validate' — it names the layer and the key
5    run 'awa doctor', then 'awa doctor --repair'; read the findings before
     assuming damage
6    another awa process holds the lock: retry, check [locks].timeout, or look
     for a stale-lock finding in 'awa doctor'
130  nothing authoritative was published; re-run — but read the message first,
     it names anything the stopped command left behind
```

Longer decision paths for each of these live in
[awa help troubleshooting](troubleshooting.md).

Note that a diagnostic command's exit status is its verdict, not an awa failure,
and two commands read the table more precisely than the one-line meanings above:

- `awa doctor` exits 5 for unrepaired errors **and** for unrepaired repairable
  findings, so a 5 can mean "there is a safe mechanical fix waiting" rather than
  damaged data. `awa run explain` is the opposite case: it executes nothing, so
  it never reports a cache verdict as a non-zero code — a non-zero exit from it
  is always awa refusing to answer (no project, invalid config, bad invocation),
  never "this would miss".
- Code 6 is for a command that could not do its job without the lock. `awa gc`
  answers two different lock questions. A writer's lock suppresses its whole
  destructive pass — nothing is deleted, the obstruction is reported as a blocked
  candidate, and it exits 0 as a normal "try again later". A second gc competing
  for the exclusive collector lease is the ordinary case instead: it waits out
  `[locks].timeout` and exits 6. So neither "gc met a busy lock" nor "gc exited 0"
  implies anything was reclaimed.

## Interruption

Ctrl+C cancels the in-flight operation through one root context: the worktree
scan, git subprocesses, GC sweep, streamed JSON, and any `awa run` child all
stop. No reusable cache hit, checkpoint, cache pointer, or GC deletion is
published from incomplete work. Non-`awa run` commands exit 130; `awa run`
instead passes through its killed child's signal-derived code (e.g. 137) and
records the interrupted run only as a non-reusable post-scan-failed history
entry that can never replay.

Nothing awa owns needs cleaning up afterwards. What a command wrote into paths YOU
named is a separate question, and re-running answers it for all but one of them: an
interrupted `awa restore` leaves the worktree part-way and re-plans from what is
there now. `awa docs export` is the exception, because its rule is that the
destination must not exist: interrupted after it created that directory, it leaves
it, names it, and tells you to remove it — the directory holds no `manifest.json`,
so it is not a valid export, and the next run refuses the path rather than resuming
into it.

## Wrapped children

`awa run` is special: when the wrapped command actually executes, or a cache hit
is replayed, awa returns the wrapped command's own exit code rather than an awa
code. A failure to write awa's own output is still reported as an awa error.

Because a wrapped child may exit with a code that overlaps awa's 1-6 range, `$?`
alone cannot distinguish an awa-origin failure from a child result. What settles
it under `--json` is the *presence of the run envelope*: an awa-origin failure is
reported on the error path with an awa code and never as a run envelope, so a
document with `data.run.exit_origin` (always `"child"`) is by construction a child
result. Without `--json`, `awa run log` identifies the execution.

## See also

- [awa help run](run.md)
- [awa help json](json.md)
- [awa help troubleshooting](troubleshooting.md)
- [exit-code reference](../reference/exit-codes.md)
