# Security Policy

## Reporting a vulnerability

Do not report a vulnerability in a public issue, and do not describe one in any other
public place in this repository.

Report it privately through GitHub instead:
[open a private security advisory](https://github.com/one-man-wolf-pack/awarer/security/advisories/new),
or reach the same form from the repository's **Security** tab under **Report a
vulnerability**. The report is visible only to you and the maintainers.

## What to include

- what an attacker can do, and what access or conditions they need to do it;
- the smallest reproduction you have, with the exact commands and the observed result;
- the output of `awa version`, and the operating system and architecture you saw it on;
- anything that narrows or widens the impact — configuration, filesystem, permissions.

Please do not include credentials, tokens, private keys, or the contents of a private
repository. Redact them and describe what was there instead. `awa` stores captured
command output verbatim under `.awa/`, so check anything you copy out of a run.

## Assessment baseline

A report is assessed against the latest published release of `awa`. Older builds and
unreleased commits are not separately maintained.

## What happens next

Reports are read and assessed, and you will hear back in the same private thread. This
project makes no response-time or remediation commitment. A fix follows the same
specification-and-review path as any other change, and details may stay private until
disclosing them is safe.
