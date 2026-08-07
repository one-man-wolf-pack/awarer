package runcache_test

import (
	"strings"
	"testing"

	"awarer/internal/domain/runcache"
)

func TestOutputCaptureValidate(t *testing.T) {
	h := newHasher(t)
	hash, err := h.HashReader(strings.NewReader("data"))
	if err != nil {
		t.Fatalf("HashReader: %v", err)
	}

	good := runcache.OutputCapture{
		OriginalBytes:    4,
		StoredBytes:      4,
		TruncationPolicy: runcache.TruncationNone,
		Hash:             hash,
		File:             "stdout.log",
	}
	if err := good.Validate(); err != nil {
		t.Errorf("valid untruncated capture rejected: %v", err)
	}

	goodTrunc := runcache.OutputCapture{
		OriginalBytes:    100,
		StoredBytes:      45, // retained head 40 + a non-empty truncation marker
		Truncated:        true,
		OmittedBytes:     60,
		TruncationPolicy: runcache.TruncationHead,
		Hash:             hash,
		File:             "stdout.log",
	}
	if err := goodTrunc.Validate(); err != nil {
		t.Errorf("valid truncated capture rejected: %v", err)
	}

	bad := []struct {
		name string
		c    runcache.OutputCapture
	}{
		{"truncated with none policy", runcache.OutputCapture{Truncated: true, OmittedBytes: 1, OriginalBytes: 1, TruncationPolicy: runcache.TruncationNone, Hash: hash, File: "f"}},
		{"truncated no omitted", runcache.OutputCapture{Truncated: true, OmittedBytes: 0, OriginalBytes: 1, TruncationPolicy: runcache.TruncationHead, Hash: hash, File: "f"}},
		{"untruncated with omitted", runcache.OutputCapture{OmittedBytes: 5, TruncationPolicy: runcache.TruncationNone, Hash: hash, File: "f"}},
		{"no file", runcache.OutputCapture{TruncationPolicy: runcache.TruncationNone, Hash: hash}},
		{"no hash", runcache.OutputCapture{TruncationPolicy: runcache.TruncationNone, File: "f"}},
		{"untruncated stored != original", runcache.OutputCapture{OriginalBytes: 4, StoredBytes: 999, TruncationPolicy: runcache.TruncationNone, Hash: hash, File: "f"}},
		{"omitted exceeds original", runcache.OutputCapture{OriginalBytes: 10, StoredBytes: 5, Truncated: true, OmittedBytes: 20, TruncationPolicy: runcache.TruncationHead, Hash: hash, File: "f"}},
		{"stored below retained head", runcache.OutputCapture{OriginalBytes: 100, StoredBytes: 10, Truncated: true, OmittedBytes: 60, TruncationPolicy: runcache.TruncationHead, Hash: hash, File: "f"}},
		{"truncated missing marker", runcache.OutputCapture{OriginalBytes: 100, StoredBytes: 40, Truncated: true, OmittedBytes: 60, TruncationPolicy: runcache.TruncationHead, Hash: hash, File: "f"}},
		{"truncated omits all", runcache.OutputCapture{OriginalBytes: 60, StoredBytes: 5, Truncated: true, OmittedBytes: 60, TruncationPolicy: runcache.TruncationHead, Hash: hash, File: "f"}},
	}
	for _, c := range bad {
		if err := c.c.Validate(); err == nil {
			t.Errorf("invalid capture %q accepted", c.name)
		}
	}
}

func TestTruncationPolicyParse(t *testing.T) {
	for _, s := range []string{"none", "head"} {
		p, err := runcache.ParseTruncationPolicy(s)
		if err != nil {
			t.Errorf("ParseTruncationPolicy(%q): %v", s, err)
		}
		if p.String() != s {
			t.Errorf("round trip %q -> %q", s, p.String())
		}
	}
	if _, err := runcache.ParseTruncationPolicy("tail"); err == nil {
		t.Error("unknown policy accepted")
	}
}
