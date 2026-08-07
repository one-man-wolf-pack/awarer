//go:build !darwin && !linux && !freebsd && !windows

package fsx

// isRenameDenied reports whether the platform refused to rename a directory this test
// holds a descriptor on.
//
// Nowhere but Windows does. A Unix-like platform renames a directory entry regardless of
// open descriptors, so the substitution the anchoring oracle injects always lands.
// Answering false is therefore the whole implementation: a rename failure here is a
// broken fixture, and the caller fails rather than reading it as the platform having
// prevented something.
func isRenameDenied(error) bool { return false }
