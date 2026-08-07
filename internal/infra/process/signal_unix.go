//go:build unix

package process

import (
	"os/exec"
	"syscall"
)

// signalInfo reports whether the process was killed by a signal and, if so, its
// name and the conventional 128+signal exit code awa returns. On Unix the signal
// is read from the wait status.
func signalInfo(ee *exec.ExitError) (name string, code int, signaled bool) {
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return "", 0, false
	}
	sig := ws.Signal()
	return sig.String(), 128 + int(sig), true
}
