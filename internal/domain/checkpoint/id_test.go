package checkpoint

import (
	"bytes"
	"crypto/rand"
	"strings"
	"testing"
)

func TestNewCheckpointIDLengthAndAlphabet(t *testing.T) {
	id, err := NewCheckpointID(rand.Reader)
	if err != nil {
		t.Fatalf("NewCheckpointID: %v", err)
	}
	if len(id.String()) != idLen {
		t.Fatalf("len = %d, want %d", len(id.String()), idLen)
	}
	for _, c := range id.String() {
		if !strings.ContainsRune(idAlphabet, c) {
			t.Fatalf("id %q has out-of-alphabet char %q", id, string(c))
		}
	}
}

func TestNewCheckpointIDDeterministicSource(t *testing.T) {
	src := bytes.Repeat([]byte{0x00}, idBytes)
	id, err := NewCheckpointID(bytes.NewReader(src))
	if err != nil {
		t.Fatalf("NewCheckpointID: %v", err)
	}
	if id.String() != strings.Repeat("0", idLen) {
		t.Fatalf("all-zero bytes -> %q, want all zeros", id)
	}
}

func TestNewCheckpointIDShortSource(t *testing.T) {
	if _, err := NewCheckpointID(bytes.NewReader([]byte{1, 2, 3})); err == nil {
		t.Fatal("expected error from short randomness source")
	}
}

func TestParseCheckpointIDRoundTrip(t *testing.T) {
	id, _ := NewCheckpointID(rand.Reader)
	got, err := ParseCheckpointID(id.String())
	if err != nil {
		t.Fatalf("ParseCheckpointID: %v", err)
	}
	if got != id {
		t.Fatalf("round trip changed id: %q -> %q", id, got)
	}
}

func TestParseCheckpointIDRejectsBad(t *testing.T) {
	cases := map[string]string{
		"too short":   strings.Repeat("a", idLen-1),
		"too long":    strings.Repeat("a", idLen+1),
		"bad char i":  strings.Repeat("a", idLen-1) + "i",
		"uppercase":   strings.Repeat("A", idLen),
		"empty":       "",
		"punctuation": strings.Repeat("a", idLen-1) + "/",
	}
	for name, s := range cases {
		if _, err := ParseCheckpointID(s); err == nil {
			t.Errorf("%s: expected error for %q", name, s)
		}
	}
}

func TestShortIsPrefix(t *testing.T) {
	id, _ := NewCheckpointID(rand.Reader)
	if !strings.HasPrefix(id.String(), id.Short()) {
		t.Fatalf("Short() %q is not a prefix of %q", id.Short(), id)
	}
	if len(id.Short()) != shortLen {
		t.Fatalf("Short len = %d, want %d", len(id.Short()), shortLen)
	}
}
