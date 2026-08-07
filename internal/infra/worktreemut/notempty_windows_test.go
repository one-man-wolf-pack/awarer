//go:build windows

package worktreemut

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

// TestNotEmptyClassifiesEachStatusDirectly exercises the Windows classification against
// stated statuses rather than only against whatever the kernel happened to return.
//
// TestNonEmptyRemovalIsClassified already drives a real non-empty removal, which is the
// stronger evidence and the reason this file does not replace it. What it cannot do is
// reach the near misses: the statuses that must NOT be read as a deletion refusal never
// arise from that one operation, so without these cases an over-broad match — fs.ErrExist,
// say, which also covers ERROR_ALREADY_EXISTS and ERROR_FILE_EXISTS — would pass
// unnoticed and turn an unrelated conflict into the one refusal that is supposed to mean
// "unobserved user content is in there".
//
// The statuses are written out here independently of the classifier, so removing one from
// isNotEmpty fails the case that names it rather than silently shrinking the test with
// it.
func TestNotEmptyClassifiesEachStatusDirectly(t *testing.T) {
	cases := []struct {
		name   string
		status syscall.Errno
		want   bool
	}{
		{"directory not empty (what RemoveDirectory returns)", syscall.ERROR_DIR_NOT_EMPTY, true},

		{"already exists", syscall.ERROR_ALREADY_EXISTS, false},
		{"file exists", syscall.ERROR_FILE_EXISTS, false},
		{"access denied", syscall.ERROR_ACCESS_DENIED, false},
		{"file not found", syscall.ERROR_FILE_NOT_FOUND, false},
		{"path not found", syscall.ERROR_PATH_NOT_FOUND, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Wrapped as os.Remove delivers it, so the classification is checked against
			// the error shape it really receives.
			err := &os.PathError{Op: "remove", Path: `C:\test`, Err: c.status}
			if got := isNotEmpty(err); got != c.want {
				t.Errorf("isNotEmpty(%v) = %v, want %v", c.status, got, c.want)
			}
		})
	}

	t.Run("a non-status error", func(t *testing.T) {
		if isNotEmpty(errors.New("some unrelated failure")) {
			t.Error("an error carrying no Win32 status was read as a deletion refusal")
		}
	})
	t.Run("no error", func(t *testing.T) {
		if isNotEmpty(nil) {
			t.Error("a nil error was read as a deletion refusal")
		}
	})
}
