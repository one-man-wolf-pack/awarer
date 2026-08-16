// Package workflows guards the two workflow boundaries no maintained tool covers:
// actionlint does not require an external action to run from an immutable commit, and
// GitHub does not stop a workflow edit from widening the reach of a credential a step
// was reviewed to hold. Everything else about these files is owned by GitHub, actionlint,
// and review.
package workflows

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const (
	workflowDir = "../../.github/workflows"
	releasePath = workflowDir + "/release.yml"
)

// actionRef captures the reference of a `uses:` step in the block form this repository
// writes. Another spelling GitHub accepts is not modeled here and is not seen by this
// scan either; review owns it, because no tool refuses it.
var actionRef = regexp.MustCompile(`^(?:- )?uses:\s*(\S+)`)

// commitPin is the only reviewable action identity: a tag can be moved to code nobody
// looked at, and these actions run in the jobs holding the repository write token and
// the Cloudflare credential.
var commitPin = regexp.MustCompile(`@[0-9a-f]{40}$`)

func TestEveryActionIsPinnedToACommit(t *testing.T) {
	found := 0
	for _, file := range workflowFiles(t) {
		for _, line := range strings.Split(readFile(t, file), "\n") {
			m := actionRef.FindStringSubmatch(strings.TrimSpace(line))
			if m == nil {
				continue
			}
			found++
			if !commitPin.MatchString(m[1]) {
				t.Errorf("%s uses %q, which is not pinned to a full commit sha", filepath.Base(file), m[1])
			}
		}
	}
	if found == 0 {
		t.Fatal("no `uses:` step found in any workflow; this check is vacuous")
	}
}

// reviewedSecretSteps binds each credential to the step reviewed to hold it, written as
// the contiguous lines release.yml must contain verbatim. Contiguous is the point: a
// token line moved verbatim into another step or job would leave a set-based or
// count-based check green, which is the failure these exist to catch.
var reviewedSecretSteps = []string{
	strings.Join([]string{
		"      - name: Upload the site to Cloudflare Pages",
		"        env:",
		"          CLOUDFLARE_API_TOKEN: ${{ secrets.CLOUDFLARE_API_TOKEN }}",
	}, "\n"),
	// The `uses:` line sits inside this block deliberately. TestEveryActionIsPinnedToACommit
	// proves a step names some immutable commit, never which one, so an edit keeping this
	// step's name and its key line while redirecting `uses:` to another pinned action would
	// hand the App private key to code nobody reviewed. The cost is that bumping this action
	// edits this block too, in the same change as the pin itself — which is the whole review
	// an `actions/*` step gets, since policy gives it no reviewed-tooling row.
	//
	// The scope lines are inside it for the same reason: this App is installed across the
	// whole organization, and the action scopes a token to every repository in that
	// installation when `owner` is set and `repositories` is not. Dropping or widening one
	// of those lines turns a tap-only credential into an organization-wide write token
	// while leaving every other check in this file green.
	strings.Join([]string{
		"      - name: Mint the tap installation token",
		"        id: tap-token",
		"        uses: actions/create-github-app-token@bcd2ba49218906704ab6c1aa796996da409d3eb1 # v3",
		"        with:",
		"          client-id: ${{ vars.RELEASE_PLEASE_APP_CLIENT_ID }}",
		"          private-key: ${{ secrets.RELEASE_PLEASE_APP_PRIVATE_KEY }}",
		"          owner: one-man-wolf-pack",
		"          repositories: homebrew-tap",
		"          permission-contents: write",
	}, "\n"),
}

// tapTokenConsumer is the other half of the tap credential's boundary: minting it under
// review is worth nothing if any later step may read it. The block runs from the step name
// to the token line because that is what makes the binding real — anchored on the token
// line alone, a step added solely to exfiltrate it would match just as well.
//
// The consumer is the tap checkout, not GoReleaser: the release assets are built with this
// repository's own token, and the cross-repository credential exists only to clone and push
// one generated formula. `repository` is inside the block because the checkout persists the
// token into whatever clone it makes, so redirecting that line would hand the credential to
// a different repository's working copy while every count here stayed green. The `uses` pin
// is inside for the reason the mint block gives.
var tapTokenConsumer = strings.Join([]string{
	"      - name: Check out the tap",
	"        uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1",
	"        with:",
	"          repository: one-man-wolf-pack/homebrew-tap",
	"          token: ${{ steps.tap-token.outputs.token }}",
}, "\n")

// releasePublisher binds the step that holds this repository's own write token. It is not a
// repository secret, so the residue scan below never sees it, and TestEveryActionIsPinnedToACommit
// proves only that some immutable commit is named — leaving `uses:` free to be redirected to
// another pinned action that would then publish this repository's releases. The version and
// args are inside for the reason the blocks above give.
var releasePublisher = strings.Join([]string{
	"      - name: Build and attach the release assets",
	"        uses: goreleaser/goreleaser-action@f06c13b6b1a9625abc9e6e439d9c05a8f2190e94 # v7.2.3",
	"        with:",
	"          distribution: goreleaser",
	"          version: v2.17.1",
	"          args: release --clean",
	"        env:",
	"          GITHUB_TOKEN: ${{ github.token }}",
}, "\n")

// TestTheDeploymentCredentialIsReachableOnlyFromTheUploader keeps publication, deployment,
// and cross-repository privilege apart: nothing outside this file stops another step from
// naming a repository secret.
func TestTheDeploymentCredentialIsReachableOnlyFromTheUploader(t *testing.T) {
	token := 0
	for _, file := range workflowFiles(t) {
		token += strings.Count(readFile(t, file), "secrets.CLOUDFLARE_API_TOKEN")
	}
	if token != 1 {
		t.Fatalf("the Cloudflare token is named %d times across the workflows, want exactly once", token)
	}

	// Two, not one: Release Please mints its own token in its own workflow, and the tap step
	// below mints the second. The count is what stops a third workflow from naming the key
	// at all — the block scan further down only reads release.yml, so without this a new
	// file could reach the key and every other check here would stay green.
	key := 0
	for _, file := range workflowFiles(t) {
		key += strings.Count(readFile(t, file), "secrets.RELEASE_PLEASE_APP_PRIVATE_KEY")
	}
	if key != 2 {
		t.Fatalf("the App private key is named %d times across the workflows, want exactly twice", key)
	}

	// The minted token is a cross-repository write credential, so it gets the same treatment
	// as the key it came from: exactly one step may read it, and that step is the tap
	// checkout. Without the count a second consumer could be added beside the reviewed one;
	// without the block the single consumer could be some other step entirely.
	minted := 0
	for _, file := range workflowFiles(t) {
		minted += strings.Count(readFile(t, file), "steps.tap-token.outputs.token")
	}
	if minted != 1 {
		t.Fatalf("the minted tap token is read %d times across the workflows, want exactly once", minted)
	}
	if !strings.Contains(readFile(t, releasePath), tapTokenConsumer) {
		t.Fatalf("release.yml does not hand the tap token to the reviewed tap checkout:\n%s", tapTokenConsumer)
	}
	if !strings.Contains(readFile(t, releasePath), releasePublisher) {
		t.Fatalf("release.yml does not publish the release from the reviewed step:\n%s", releasePublisher)
	}

	// Each reviewed block appears verbatim, and once each is removed nothing left in
	// release.yml names a secret at all. A duplicated block survives its own single removal,
	// so the scan reports it.
	residue := readFile(t, releasePath)
	for _, step := range reviewedSecretSteps {
		if !strings.Contains(residue, step) {
			t.Fatalf("release.yml does not carry this reviewed secret-holding step:\n%s", step)
		}
		residue = strings.Replace(residue, step, "", 1)
	}
	for _, line := range strings.Split(residue, "\n") {
		if strings.Contains(line, "secrets.") {
			t.Errorf("a step outside the reviewed ones names a secret: %s", strings.TrimSpace(line))
		}
	}
}

// workflowFiles lists both extensions GitHub accepts, so a workflow added as *.yaml
// cannot carry an unpinned action or a second credential past these checks.
func workflowFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, pattern := range []string{"*.yml", "*.yaml"} {
		matched, err := filepath.Glob(filepath.Join(workflowDir, pattern))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, matched...)
	}
	if len(out) == 0 {
		t.Fatalf("no workflows under %s; this check is vacuous", workflowDir)
	}
	return out
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}
