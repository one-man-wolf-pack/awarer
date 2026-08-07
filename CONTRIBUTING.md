# Contributing to awa

Thank you for looking at how `awa` behaves and where it falls short. Reports and
proposals are genuinely useful, and they are how this project improves.

`awa` is issue-driven: issues are the contribution channel, and external pull requests
are not accepted. Reading the rest of this page takes a minute and can save you an
afternoon.

## What we welcome

Open an issue for any of these:

- a reproducible defect, especially anything that corrupts, loses, or misreports local
  evidence under `.awa/`;
- a real workflow `awa` handles badly, or one it does not cover at all;
- a feature or use-case proposal, described as the problem it would solve;
- performance and portability observations — slow paths, memory behavior, or an
  operating system or filesystem where `awa` misbehaves;
- integration friction with your shell, editor, CI, or coding agent;
- documentation that is missing, wrong, or misleading, including the built-in help and
  the public site;
- design criticism, including disagreement with a decision `awa` has already made.

Concrete evidence is worth more than a proposed fix. The two forms ask for what is
needed to make a decision: [report a defect or documentation problem][bug], or
[propose a feature or describe a use case][feature].

An accepted issue is not a promise. It does not commit the project to implementing
anything, to a timeline, or to the design you proposed; an issue may be reframed,
combined, deferred, declined, or solved a different way.

## Why we do not accept pull requests

We do not accept external pull requests — code, tests, documentation, workflows, and
repository maintenance alike. This is not a judgment about you, your patch, or how it
was written. It follows from how the project is built.

`awa` is specified before it is implemented. Behavior, contracts, boundaries, and
verification are decided first, in a maintainer-owned internal design and review
process; the code here is then written and reviewed against that frozen design, and the
maintainer accepts it. A patch that arrives already written has skipped the step that
decides what the software should do, so there is no point in this process where it
could be reviewed and merged.

There is no exception process, so please do not open a pull request expecting a review.

## What happens to an issue

1. You report a problem, use case, reproduction, or observation.
2. It is triaged as evidence, and you may be asked for facts that are missing.
3. If it is accepted, its consequences are designed internally: contracts,
   boundaries, compatibility, and how the result will be verified.
4. That design is frozen into a work item for implementation.
5. It is implemented and reviewed, and the maintainer accepts and lands it.

Your issue is the input to step 3. That is why the forms ask for the problem and the
evidence rather than for a design.

## Forks

`awa` is under the [Apache License 2.0](LICENSE). You are free to copy, modify, and
redistribute it under that license, and a fork you maintain for yourself is a perfectly
good outcome.

You are also welcome to link a patch or prototype from an issue as evidence — it can
show that a problem is solvable, or make a use case concrete. It will not be reviewed
or merged as an upstream contribution.

There is no contributor license agreement, developer certificate of origin, or sign-off
to complete, because there is no patch intake to attach one to.

## Security

Do not report a vulnerability in a public issue. [`SECURITY.md`](SECURITY.md) has the
private reporting route.

[bug]: https://github.com/one-man-wolf-pack/awarer/issues/new?template=bug-report.yml
[feature]: https://github.com/one-man-wolf-pack/awarer/issues/new?template=feature-request.yml
