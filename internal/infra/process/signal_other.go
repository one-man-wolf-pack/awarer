//go:build !unix

package process

import "os/exec"

// signalInfo reports no signal on platforms without a wait-status mapping; the
// exit code path still reports the process's code. Signal capture is a best-effort
// Unix feature, so other platforms degrade to a normal exit rather than failing.
func signalInfo(_ *exec.ExitError) (name string, code int, signaled bool) {
	return "", 0, false
}
