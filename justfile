# awa developer tasks. Bare `just` lists the recurring workflows, not an inventory of
# what this repository can do; focused one-step work keeps its native owner.
#
# Recipes prefixed with `_` are internal: they stay out of `just --list` while remaining
# directly callable, which is how .github/workflows invokes a lane. The aggregates below
# and CI compose the same internal recipes, so locally-green means CI-green by shared
# implementation rather than by two command lists that agree today.
#
# Analysis tools run through `go run` at a pinned version, so they need no separate
# install and analyze with a build matching the local Go toolchain — a mismatched
# system binary that silently analyzes nothing is the failure the guards below catch.

set dotenv-load := false

minimum-version := "1.57.0"
_ := assert(semver_matches(just_version(), ">=" + minimum-version) == "true", "just " + minimum-version + " or newer is required")

staticcheck-version := "v0.8.0"
golangci-version := "v2.12.2"
# govulncheck pins only the analyzer binary; it still fetches the current
# vulnerability database at run time, so a fixed version does not stale the data.
govulncheck-version := "v1.6.0"
# actionlint validates the workflow YAML and the Blacksmith runner labels declared
# in .github/actionlint.yaml.
actionlint-version := "v1.7.12"

# Configurable paths and identities are environment values, exported here with their
# defaults and read inside recipes as "$VAR". They are never spliced into a recipe with
# {{ }}: an interpolated value becomes shell source, so a path holding a double quote or
# a URL holding a backtick would run as shell. The rule holds for every value — an
# exception is a thing somebody has to know about.
#
# A fixed generated artifact is the other case: it is a literal inside the recipes that
# own it rather than an exported value, because it is not configurable at all. A path
# nobody can supply is a path nobody can redirect, which is what lets `site` and `clean`
# remove it with an ordinary recursive delete.

# The committed golden the generated CLI reference is checked against.
export REFERENCE_GOLDEN := env("REFERENCE_GOLDEN", "internal/cli/testdata/reference.json")

# The reviewed third-party license manifest and the notice it generates.
export LICENSE_MANIFEST := env("LICENSE_MANIFEST", "third_party/licenses.json")
export THIRD_PARTY_NOTICES := env("THIRD_PARTY_NOTICES", "THIRD_PARTY_NOTICES")

# The public site. AWA_BIN selects the binary whose documentation the site projects;
# empty means "build one from this tree", which is a development preview and is marked
# as one on every page. A production build passes the path of a binary extracted from the
# completed release's own archive and sets SITE_RELEASE=1 — that is the whole difference
# between a preview and a publishable site, and it is why nothing here reads git, a tag,
# or the network to decide.
export SITE_BASE_URL := env("SITE_BASE_URL", "https://awarer.one-man-wolf-pack.com")
export SITE_SERVE_ADDR := env("SITE_SERVE_ADDR", "127.0.0.1:8080")
export AWA_BIN := env("AWA_BIN", "")
export SITE_RELEASE := env("SITE_RELEASE", "")

[private]
default:
    @just --list

# ── Tests ────────────────────────────────────────────────────────────────────

# Run the Go test suite.
[group('Tests')]
test:
    go test ./...

# Run the high-concurrency, high-interruption acceptance scenarios.
[doc('Run the stress-tagged acceptance scenarios (slow, scheduler-sensitive, not part of `check`).')]
[group('Tests')]
stress:
    go test -tags acceptance_stress -count=1 ./test/acceptance/...

# ── Quality ──────────────────────────────────────────────────────────────────

# The fast gate: formatting, vet, tests, race, lint, generated reference, licences.
[group('Quality')]
check: _gate _licenses-fast

# ── Site ─────────────────────────────────────────────────────────────────────

# Build the public site into site/dist from a fresh export of the selected binary.
#
# site/dist is removed and rebuilt as one tree. sitegen writes only into a directory that
# does not exist and owns no removal of its own, so the removal is this recipe's, and it
# is a literal path: nothing supplied to Just can redirect a recursive delete.
#
# Deployment is deliberately not here — this needs no token, no network, and no Node
# runtime.
[doc('Build the public site into site/dist from a fresh export of the selected binary.')]
[group('Site')]
site:
    #!/usr/bin/env bash
    set -euo pipefail
    tmp=$(mktemp -d)
    trap 'rm -rf "$tmp"' EXIT
    bin="$AWA_BIN"
    if [ -z "$bin" ]; then
        # Release mode drops the development-preview banner, so a build from this tree
        # would publish a page claiming to be a release of 0.0.0-dev. Refuse rather than
        # fall back: the caller asked for the published configuration.
        if [ -n "$SITE_RELEASE" ]; then
            echo 'site: SITE_RELEASE is set but AWA_BIN is empty — refusing to render a development build as a release' >&2
            exit 1
        fi
        bin="$tmp/awa"
        go build -o "$bin" ./cmd/awa
    fi
    "$bin" docs export --output "$tmp/export" >/dev/null
    # Last, so a failed build or export leaves the previous site standing. What a
    # generator failure can leave is a partial tree that means nothing; the next run
    # removes it here, and nothing serves or uploads one in between.
    rm -rf site/dist
    go run ./site/sitegen --docs "$tmp/export" --output site/dist \
        --base-url "$SITE_BASE_URL" ${SITE_RELEASE:+--release}

# Serve the built site for local inspection (loopback only, read-only, not a deployment path).
[group('Site')]
site-serve:
    go run ./site/siteserve -dir site/dist -addr "$SITE_SERVE_ADDR"

# ── Generated artifacts ──────────────────────────────────────────────────────

# Regenerate the committed CLI reference golden after an intended surface change.
#
# The generator builds and validates the document in memory, then publishes it
# atomically, so a failed build or generation leaves the existing golden untouched.
[doc('Regenerate the committed CLI reference golden after an intended surface change.')]
[group('Generated')]
reference-update:
    @go run ./internal/tools/refgen -output "$REFERENCE_GOLDEN"
    @echo "reference-update: wrote $REFERENCE_GOLDEN"

# Regenerate THIRD_PARTY_NOTICES after a reviewed licence-manifest change.
[group('Generated')]
notices-update:
    @go run ./internal/tools/liccheck -mode update \
        -manifest "$LICENSE_MANIFEST" -output "$THIRD_PARTY_NOTICES"

# ── Release ──────────────────────────────────────────────────────────────────

[doc('Prove this commit is releasable: the complete pre-release gate.')]
[group('Release')]
release-gate: _gate _licenses-full _analyze stress _crossbuild _smoke
    @echo "release-gate OK"

# ── Maintenance ──────────────────────────────────────────────────────────────

# Remove the built binary, the generated site, and the release artifacts.
#
# Every path here is a fixed literal this repository generates: site/dist is what `site`
# writes, and dist is GoReleaser's own. Neither takes a caller-supplied value, so nothing
# a stray environment variable can point elsewhere reaches a recursive remove.
[doc('Remove the built binary, the generated site, and the release artifacts.')]
[group('Maintenance')]
clean:
    rm -f awa
    rm -rf site/dist dist
    go clean

# ── Internal lanes (hidden from `just --list`, called by aggregates and CI) ───

# The always-available core both `check` and `release-gate` run.
#
# Cheapest first: a missing gofmt should not cost a full test run to discover. `test` is
# invoked rather than declared as a dependency for that reason — a Just dependency would
# run it before this body, ahead of every check that exists to fail first — while keeping
# the public recipe the one owner of the suite command.
_gate:
    #!/usr/bin/env bash
    set -euo pipefail
    out=$(gofmt -l .)
    if [ -n "$out" ]; then echo "gofmt needed:"; echo "$out"; exit 1; fi
    # This file is held to the same formatting gate as the Go sources.
    just --fmt --check >/dev/null || {
        echo 'justfile is not formatted; run: just --fmt'
        exit 1
    }
    go vet ./...
    just test
    go test -race ./...
    # golangci-lint logs "no go files to analyze" and still exits 0 in some versions;
    # treat loading zero packages as a failure rather than a silent pass.
    out=$(go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@{{ golangci-version }} run 2>&1) || {
        printf '%s\n' "$out"; exit 1
    }
    if [ -n "$out" ]; then printf '%s\n' "$out"; fi
    if printf '%s' "$out" | grep -qi 'no go files to analyze'; then
        echo 'golangci-lint: no go files to analyze — refusing to pass without analyzing the project'
        exit 1
    fi
    go run ./internal/tools/refgen -check -golden "$REFERENCE_GOLDEN"

# Licence compliance, in the two scopes the gates use. `fast` validates the manifest,
# every source-text digest, and the committed notice against the single host target;
# `full` adds the cross-target reachability union and target-set drift, which is too
# slow for the frequently-run fast gate.
#
# Two recipes rather than one taking the scope as a parameter: a recipe parameter is
# interpolated into the recipe's shell text, so `just _licenses 'fast; …'` would run the
# tail as shell before liccheck ever saw the value. The only scopes that exist are these
# two literals.
_licenses-fast:
    @go run ./internal/tools/liccheck -mode check -scope fast \
        -manifest "$LICENSE_MANIFEST" -notices "$THIRD_PARTY_NOTICES"

_licenses-full:
    @go run ./internal/tools/liccheck -mode check -scope full \
        -manifest "$LICENSE_MANIFEST" -notices "$THIRD_PARTY_NOTICES"

# The pinned external analyzers, in one lane. Each guard exists because the tool
# exits 0 after analyzing nothing — a tool/toolchain mismatch, a moved workflow
# directory — and a gate that passes without having analyzed the project is worse
# than no gate.
_analyze:
    #!/usr/bin/env bash
    set -euo pipefail
    out=$(go run honnef.co/go/tools/cmd/staticcheck@{{ staticcheck-version }} ./... 2>&1) || {
        printf '%s\n' "$out"; exit 1
    }
    if [ -n "$out" ]; then printf '%s\n' "$out"; fi
    if printf '%s' "$out" | grep -q 'matched no packages'; then
        echo 'staticcheck: matched no packages — refusing to pass without analyzing the project'
        exit 1
    fi

    out=$(go run golang.org/x/vuln/cmd/govulncheck@{{ govulncheck-version }} ./... 2>&1) || {
        printf '%s\n' "$out"; exit 1
    }
    printf '%s\n' "$out"
    if [ -z "$out" ]; then
        echo 'govulncheck: produced no output — refusing to pass without analyzing the project'
        exit 1
    fi

    # actionlint auto-discovers .github/workflows; a moved or misnamed path must fail
    # the gate, not pass it by analyzing nothing.
    if [ -z "$(ls .github/workflows/*.yml .github/workflows/*.yaml 2>/dev/null)" ]; then
        echo 'actionlint: no workflow files under .github/workflows — refusing to pass'
        exit 1
    fi
    go run github.com/rhysd/actionlint/cmd/actionlint@{{ actionlint-version }}

# Cross-target compile evidence in two passes: ./cmd/awa is built for every release
# target, then the full package and test graph is type-checked for each release operating
# system this host is not. Nothing here runs and every output is discarded.
#
# The target list is early compile feedback only, so it is kept here rather than derived:
# `.goreleaser.yaml` decides what is built and published, and the licence tool's own list
# decides what the licence union covers. Each fails at its own consequence boundary.
#
# The second pass exists because the first reaches no test file. The platform-only tests
# of the systems this host never selects would otherwise first be compiled by the remote
# lane that gates a release; `go vet` type-checks the package and test graph, so this
# turns that into a local failure instead.
_crossbuild:
    #!/usr/bin/env bash
    set -euo pipefail
    targets='darwin/amd64 darwin/arm64 freebsd/amd64 linux/amd64 linux/arm64 windows/amd64'
    for t in $targets; do
        echo "build $t"
        GOOS="${t%/*}" GOARCH="${t#*/}" go build -o /dev/null ./cmd/awa
    done
    checked=0
    for t in $targets; do
        case "${t%/*}" in
        darwin | linux) continue ;;
        esac
        echo "type-check $t (packages and tests)"
        GOOS="${t%/*}" GOARCH="${t#*/}" go vet ./...
        checked=$((checked + 1))
    done
    if [ "$checked" -eq 0 ]; then
        echo 'crossbuild: no target outside the host operating systems — refusing to pass without type-checking any platform-only test'
        exit 1
    fi
    echo "crossbuild OK"

# Build the real binary and drive it through the release smoke scenarios.
_smoke:
    go test -tags acceptance_smoke -count=1 -run '^TestSmoke' ./test/acceptance/...
