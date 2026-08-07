// Package workflows guards the two workflow boundaries no maintained tool covers:
// actionlint does not require an external action to run from an immutable commit, and
// GitHub does not stop a workflow edit from widening the reach of the deployment
// credential. Everything else about these files is owned by GitHub, actionlint, and
// review.
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

// uploadStep binds the deployment credential to the step reviewed to hold it. Contiguous
// is the point: the token line moved verbatim into another job would leave a set-based
// check green, which is the failure this exists to catch.
var uploadStep = strings.Join([]string{
	"      - name: Upload the site to Cloudflare Pages",
	"        env:",
	"          CLOUDFLARE_API_TOKEN: ${{ secrets.CLOUDFLARE_API_TOKEN }}",
}, "\n")

// TestTheDeploymentCredentialIsReachableOnlyFromTheUploader keeps publication and
// deployment privilege apart: nothing outside this file stops another step from naming a
// repository secret. Release Please carries its own GitHub App key in its own workflow.
// site/DEPLOYMENT.md names this test as the enforcement, so the name is part of it.
func TestTheDeploymentCredentialIsReachableOnlyFromTheUploader(t *testing.T) {
	token := 0
	for _, file := range workflowFiles(t) {
		token += strings.Count(readFile(t, file), "secrets.CLOUDFLARE_API_TOKEN")
	}
	if token != 1 {
		t.Fatalf("the Cloudflare token is named %d times across the workflows, want exactly once", token)
	}

	release := readFile(t, releasePath)
	if !strings.Contains(release, uploadStep) {
		t.Fatalf("release.yml does not carry the reviewed upload step:\n%s", uploadStep)
	}
	for _, line := range strings.Split(strings.Replace(release, uploadStep, "", 1), "\n") {
		if strings.Contains(line, "secrets.") {
			t.Errorf("a step outside the upload names a secret: %s", strings.TrimSpace(line))
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
