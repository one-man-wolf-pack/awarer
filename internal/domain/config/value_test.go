package config

import (
	"testing"
	"time"
)

func TestParseByteSize(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"50MiB", 50 * miB, false},
		{"100MiB", 100 * miB, false},
		{"1KiB", kiB, false},
		{"2GiB", 2 * giB, false},
		{"1TiB", tiB, false},
		{"1024B", 1024, false},
		{"1024", 1024, false},
		{" 50MiB ", 50 * miB, false},
		{"", 0, true},
		{"-5MiB", 0, true},
		{"5GB", 0, true},           // decimal units are not supported
		{"abc", 0, true},           // not a number
		{"MiB", 0, true},           // missing number
		{"8388609TiB", 0, true},    // overflows int64 when scaled
		{"9999999999GiB", 0, true}, // overflows int64 when scaled
	}
	for _, tt := range tests {
		got, err := ParseByteSize(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseByteSize(%q) = %d, want error", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseByteSize(%q) unexpected error: %v", tt.in, err)
			continue
		}
		if int64(got) != tt.want {
			t.Errorf("ParseByteSize(%q) = %d, want %d", tt.in, int64(got), tt.want)
		}
	}
}

func TestByteSizeString(t *testing.T) {
	tests := []struct {
		in   ByteSize
		want string
	}{
		{50 * miB, "50MiB"},
		{100 * miB, "100MiB"},
		{kiB, "1KiB"},
		{0, "0B"},
		{1, "1B"},
		{1023, "1023B"},
	}
	for _, tt := range tests {
		if got := tt.in.String(); got != tt.want {
			t.Errorf("ByteSize(%d).String() = %q, want %q", int64(tt.in), got, tt.want)
		}
	}
}

func TestByteSizeRoundTrip(t *testing.T) {
	for _, s := range []string{"50MiB", "100MiB", "1KiB", "2GiB"} {
		bs, err := ParseByteSize(s)
		if err != nil {
			t.Fatalf("ParseByteSize(%q): %v", s, err)
		}
		if got := bs.String(); got != s {
			t.Errorf("round trip %q -> %q", s, got)
		}
	}
}

func TestParseDuration(t *testing.T) {
	tests := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"7d", 7 * 24 * time.Hour, false},
		{"14d", 14 * 24 * time.Hour, false},
		{"12h", 12 * time.Hour, false},
		{"30m", 30 * time.Minute, false},
		{"45s", 45 * time.Second, false},
		{"", 0, true},
		{"7", 0, true}, // no unit
		{"-3d", 0, true},
		{"3y", 0, true}, // unknown unit
		{"abc", 0, true},
		{"106752d", 0, true}, // overflows int64 nanoseconds when scaled
	}
	for _, tt := range tests {
		got, err := ParseDuration(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseDuration(%q) = %v, want error", tt.in, time.Duration(got))
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseDuration(%q) unexpected error: %v", tt.in, err)
			continue
		}
		if time.Duration(got) != tt.want {
			t.Errorf("ParseDuration(%q) = %v, want %v", tt.in, time.Duration(got), tt.want)
		}
	}
}

func TestDurationString(t *testing.T) {
	for _, s := range []string{"7d", "14d", "12h", "30m", "45s"} {
		d, err := ParseDuration(s)
		if err != nil {
			t.Fatalf("ParseDuration(%q): %v", s, err)
		}
		if got := d.String(); got != s {
			t.Errorf("round trip %q -> %q", s, got)
		}
	}
}

func TestParseEnums(t *testing.T) {
	if m, err := ParseTrustMode("strict"); err != nil || m != TrustStrict {
		t.Errorf("ParseTrustMode(strict) = %v, %v", m, err)
	}
	if _, err := ParseTrustMode("paranoid"); err == nil {
		t.Error("ParseTrustMode(paranoid) should error")
	}
	if p, err := ParseLargeFilePolicy("hash-only"); err != nil || p != LargeFileHashOnly {
		t.Errorf("ParseLargeFilePolicy(hash-only) = %v, %v", p, err)
	}
	if _, err := ParseLargeFilePolicy("ignore"); err == nil {
		t.Error("ParseLargeFilePolicy(ignore) should error")
	}
}

func TestEnumStringRoundTrip(t *testing.T) {
	for _, s := range []string{"normal", "strict", "fast"} {
		m, err := ParseTrustMode(s)
		if err != nil || m.String() != s {
			t.Errorf("trust mode round trip %q -> %q (%v)", s, m.String(), err)
		}
	}
	for _, s := range []string{"hash-only", "store", "skip"} {
		p, err := ParseLargeFilePolicy(s)
		if err != nil || p.String() != s {
			t.Errorf("large file policy round trip %q -> %q (%v)", s, p.String(), err)
		}
	}
}
