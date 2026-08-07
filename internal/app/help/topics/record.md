# awa run --record — supervised runs

```text
awa run --record [flags] -- <command> [args...]
```

Use `--record` for commands that must not be cached but whose execution still
matters: deployments, migrations, Ansible playbooks, formatters, long
validations, and anything whose log you may need later.

`--record` always executes the command. It never reads an existing cache hit and
never publishes a reusable hit. It captures stdout and stderr, the exit code or
terminating signal, duration, the working directory, the command line, and the
observed before/after state around the run — as durable history.

```text
awa run --record --display tail:200 -- ansible-playbook site.yml
awa run show --last --tail 500                 # inspect the recorded run
awa changes run:<id>:before..run:<id>:after    # what the command changed
awa run log                                    # every recorded run
awa log --all                                  # full timeline: checkpoints
                                               # plus recorded runs
```

Even an ordinary `awa run` records a mutating or failed execution as history;
only a clean, non-mutating run becomes reusable. Reach for `--record` when you
know up front that caching is unsafe but you still want durable evidence.

## Long-running and noisy commands

A recorded run is the better shape for a long deploy, migration, or full test
sweep than redirecting to a scratch file. Bound what reaches the terminal while
the full log is still stored:

```text
awa run --record --display tail:200 -- <command>
awa run --record --display none -- <command>
```

`<command>` is the invocation to supervise. `--display` affects this terminal
only — never what is captured, and never the exit code, which is the wrapped
command's own whenever the command actually ran (see
[awa help exit-codes](exit-codes.md) for the one awa-origin exception).
Interrupting a recorded run leaves a non-reusable history entry rather than a
partial result that could later replay.

## What a record proves, and what it does not

A record proves what that execution printed, when it ran, from which directory,
with which exit status, and how the observed state differed before and after it.

It does not prove that running the command again now would do the same thing,
and it is never a cache hit: a recorded run carries the reuse reason
`record-only` and is never offered for replay, however clean it was. Nor does it
capture anything awa does not observe — network services, databases, wall-clock
behavior, and files outside the project root are all outside the evidence.

## Evidence of what changed

The observed before/after states are addressable — for every stored run, not only
a recorded one — which is how you check the effect rather than trusting the log:

```text
awa changes run:<id>:before..run:<id>:after
awa diff run:<id>:before..run:<id>:after
```

`<id>` is the run id printed in the footer. Ignored and protected paths are
outside the observed scope, so a change confined to them does not appear here.

## See also

- [awa help run](run.md)
- [awa help inspect](inspect.md)
- [awa help diff](diff.md)
- [awa help refs](refs.md)
- [awa help workflows](workflows.md)
