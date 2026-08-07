# Deploying the site

Publishing a release publishes its documentation site. There is no separate
deployment workflow and no manual trigger: the `site` job in
[`.github/workflows/release.yml`](../.github/workflows/release.yml) runs once that
release's assets are attached. Ordinary pushes and pull requests never deploy.

The job downloads the released `linux/amd64` archive, generates the site from that
binary's own documentation export, and hands the directory to one Wrangler upload. It is
best-effort: the release is already complete when it starts, so a failure means a stale
site, never an invalid release.

The public site is `https://awarer.one-man-wolf-pack.com`.

## One-time setup

Repository variables:

| Name | Value |
| --- | --- |
| `CLOUDFLARE_ACCOUNT_ID` | Account containing the Pages project. |
| `CLOUDFLARE_PAGES_PROJECT` | Direct Upload Pages project name. |

Repository secret `CLOUDFLARE_API_TOKEN`, scoped to only **Account → Cloudflare
Pages → Edit**. Only the upload step references it, and that is the one thing here a
test does enforce: `TestTheDeploymentCredentialIsReachableOnlyFromTheUploader` fails if
the secret appears outside that step.

In Cloudflare:

- one Direct Upload Pages project, production branch `main` — Wrangler is invoked
  with `--branch=main`, and a project on any other production branch takes each
  upload as a preview instead;
- the custom hostname `awarer.one-man-wolf-pack.com` attached to that project.

### Reviewed deployment tools

These pins are a human review record, not a generated inventory: nothing reconciles them
with the workflow, so a bump edits both in the same change.

| Tool | Pin | Upstream | License | Reviewed |
| --- | --- | --- | --- | --- |
| `actions/setup-node` | `820762786026740c76f36085b0efc47a31fe5020` (v7.0.0) | https://github.com/actions/setup-node | `MIT` | 2026-08-04 |
| `wrangler` (npm) | `4.118.0` | https://github.com/cloudflare/workers-sdk | `MIT OR Apache-2.0` | 2026-08-04 |

Wrangler is fetched by `npx --yes wrangler@4.118.0` at the upload step rather than
installed as a repository dependency; `setup-node` only supplies the interpreter it
needs. When either pin changes, review the selected tool and license and update this
table in the same change.

### Manual audit items

Git cannot prove these remote settings. Check them at setup and after access or
workflow changes:

1. `CLOUDFLARE_API_TOKEN` grants Pages edit and nothing else.
2. The Pages production branch is `main`.
3. `awarer.one-man-wolf-pack.com` is attached to that project and to no other.
4. New or bumped external actions have an explicit pin and license review.

## Deploying

Nothing to do. Publish a release; the site follows. Watch the `site` job of that
release's **Release** run.

Wrangler's successful exit is the evidence that Cloudflare accepted the directory.
Nothing polls the public hostname, compares the served release, or probes routes —
verify by opening the site if you want to.

## Failures And Reruns

Read the first failing named step:

- **Fetch the released binary** — the expected archive is not attached to that release
  under its published name, or the download was refused.
- **just site** — the generator refused the release's export or failed to render it: an
  unparseable manifest, a declared document that is missing, an unresolved internal link
  or anchor, an invalid base URL, or a write failure. The job stops here, so a partial
  tree is never uploaded, and the rerun rebuilds `site/dist` from nothing.
- **Upload the site to Cloudflare Pages** — account, project, credential, or Pages
  state is wrong.

After correcting the named condition, choose **Re-run failed jobs** on that **Release**
run so only `site` runs again. **Re-run all jobs** also re-runs the release job, which
fails on assets that are already attached — loudly, and without changing them. The
release itself is untouched by any of this.

A rerun checks out the release tag, not `main`, so it repairs only conditions outside
the checkout: Cloudflare state, the repository variables and secret, a missing asset, or
an infrastructure failure. A **just site** failure caused by the generator or the
released export is in the tagged tree and stays there — it is fixed on `main` and
published by the next release.

A rerun creates another immutable Cloudflare deployment; Cloudflare's deployment
history is the inventory, and nothing in this repository reconciles it. A site left
stale until the next release is an accepted outcome, not an incident.

## Rollback

Rollback is a Cloudflare operation. List deployments when needed:

```bash
npx wrangler@4.118.0 pages deployment list --project-name=<project>
```

In Cloudflare, open **Workers & Pages → project → Deployments**, select the
target production deployment, and choose **Rollback**.
