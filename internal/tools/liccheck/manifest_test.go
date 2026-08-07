package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testDigest = "blake3:" + "1111111111111111111111111111111111111111111111111111111111111111"

// validEntry is a complete, policy-clean production row. Each policy case below
// mutates exactly one field of a copy, so a rejection can only be that field.
func validEntry() rawEntry {
	return rawEntry{
		ModulePath:  "example.com/x",
		Version:     "v1.0.0",
		Targets:     []string{"linux/amd64"},
		SPDX:        "MIT",
		Texts:       []rawText{{Path: "LICENSE", Role: "license", Digest: testDigest, MustShip: true}},
		Disposition: "must_appear_in_binary_and_source",
	}
}

func validManifest() rawManifest {
	return rawManifest{
		SchemaVersion:   1,
		ProjectLicense:  rawProjectLicense{SPDX: "Apache-2.0", Path: "LICENSE", Digest: testDigest},
		CopyrightHolder: "Zada Zorg",
		Entries:         []rawEntry{validEntry()},
	}
}

// TestManifestPolicy is the strict production policy in one place: everything the
// reviewed manifest may say, and everything it may not. The clean fixture must pass,
// so each rejection below is attributable to the single field the case changed.
func TestManifestPolicy(t *testing.T) {
	if _, vs := parseManifest(validManifest()); len(vs) != 0 {
		t.Fatalf("the clean fixture must pass, otherwise no case below proves anything: %v", vs)
	}

	cases := []struct {
		name string
		want string // the rule the manifest must be rejected under
		edit func(*rawManifest)
	}{
		{"wrong schema version", ruleManifestHeader, func(m *rawManifest) { m.SchemaVersion = 2 }},
		{"absent schema version", ruleManifestHeader, func(m *rawManifest) { m.SchemaVersion = 0 }},
		{"empty copyright holder", ruleManifestHeader, func(m *rawManifest) { m.CopyrightHolder = "" }},

		{"project license unapproved", ruleProjectLicense, func(m *rawManifest) { m.ProjectLicense.SPDX = "WTFPL" }},
		{"project license absolute path", ruleProjectLicense, func(m *rawManifest) { m.ProjectLicense.Path = "/etc/LICENSE" }},
		{"project license bad digest", ruleProjectLicense, func(m *rawManifest) { m.ProjectLicense.Digest = "sha256:abc" }},

		{"unapproved spdx atom", ruleUnapprovedSPDX, func(m *rawManifest) { m.Entries[0].SPDX = "GPL-3.0" }},
		{"unknown spdx operator", ruleUnapprovedSPDX, func(m *rawManifest) { m.Entries[0].SPDX = "MIT WITH Exception" }},
		{"trailing spdx operator", ruleUnapprovedSPDX, func(m *rawManifest) { m.Entries[0].SPDX = "MIT AND" }},
		{"empty spdx", ruleUnapprovedSPDX, func(m *rawManifest) { m.Entries[0].SPDX = "" }},

		{"empty module path", ruleMalformedEntry, func(m *rawManifest) { m.Entries[0].ModulePath = "" }},
		{"empty version", ruleMalformedEntry, func(m *rawManifest) { m.Entries[0].Version = "" }},
		{"non-go version", ruleMalformedEntry, func(m *rawManifest) { m.Entries[0].Version = "1.0.0" }},
		{"local replacement", ruleMalformedEntry, func(m *rawManifest) {
			m.Entries[0].Replacement = &rawReplacement{ModulePath: "../local"}
		}},
		{"replacement without version", ruleMalformedEntry, func(m *rawManifest) {
			m.Entries[0].Replacement = &rawReplacement{ModulePath: "example.com/y", Version: ""}
		}},

		{"no targets", ruleMalformedEntry, func(m *rawManifest) { m.Entries[0].Targets = nil }},
		{"target without arch", ruleMalformedEntry, func(m *rawManifest) { m.Entries[0].Targets = []string{"linux"} }},
		{"target with empty half", ruleMalformedEntry, func(m *rawManifest) { m.Entries[0].Targets = []string{"linux/"} }},
		{"target with spaces", ruleMalformedEntry, func(m *rawManifest) { m.Entries[0].Targets = []string{"li nux/amd64"} }},
		{"repeated target", ruleMalformedEntry, func(m *rawManifest) {
			m.Entries[0].Targets = []string{"linux/amd64", "linux/amd64"}
		}},

		{"no texts", ruleMalformedEntry, func(m *rawManifest) { m.Entries[0].Texts = nil }},
		{"unknown text role", ruleMalformedEntry, func(m *rawManifest) { m.Entries[0].Texts[0].Role = "readme" }},
		{"empty text path", ruleMalformedEntry, func(m *rawManifest) { m.Entries[0].Texts[0].Path = "" }},
		{"absolute text path", ruleMalformedEntry, func(m *rawManifest) { m.Entries[0].Texts[0].Path = "/etc/passwd" }},
		{"escaping text path", ruleMalformedEntry, func(m *rawManifest) { m.Entries[0].Texts[0].Path = "../escape" }},
		{"unclean text path", ruleMalformedEntry, func(m *rawManifest) { m.Entries[0].Texts[0].Path = "./LICENSE" }},
		{"malformed digest", ruleMalformedEntry, func(m *rawManifest) { m.Entries[0].Texts[0].Digest = "blake3:zz" }},
		{"empty digest", ruleMalformedEntry, func(m *rawManifest) { m.Entries[0].Texts[0].Digest = "" }},

		{"unshipped license text", ruleUnshippedLicense, func(m *rawManifest) { m.Entries[0].Texts[0].MustShip = false }},
		{"unshipped patents text", ruleUnshippedLicense, func(m *rawManifest) {
			m.Entries[0].Texts = append(m.Entries[0].Texts, rawText{Path: "PATENTS", Role: "patents", Digest: testDigest})
		}},
		{"unshipped third-party text", ruleUnshippedLicense, func(m *rawManifest) {
			m.Entries[0].Texts = append(m.Entries[0].Texts, rawText{Path: "NOTICE-3RD", Role: "third_party", Digest: testDigest})
		}},

		{"unknown disposition", ruleMissingDisposition, func(m *rawManifest) { m.Entries[0].Disposition = "ship_it" }},
		{"empty disposition", ruleMissingDisposition, func(m *rawManifest) { m.Entries[0].Disposition = "" }},
		{"ships nothing but claims it must", ruleDispositionMismatch, func(m *rawManifest) {
			m.Entries[0].Texts = []rawText{{Path: "AUTHORS", Role: "authors", Digest: testDigest}}
		}},
		{"claims no redistribution but ships", ruleDispositionMismatch, func(m *rawManifest) {
			m.Entries[0].Disposition = "no_redistribution_text"
		}},

		{"duplicate module", ruleDuplicateEntry, func(m *rawManifest) { m.Entries = append(m.Entries, validEntry()) }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := validManifest()
			tc.edit(&raw)
			_, vs := parseManifest(raw)
			if len(vs) == 0 {
				t.Fatalf("must be rejected, but the manifest passed")
			}
			for _, v := range vs {
				if v.rule == tc.want {
					return
				}
			}
			t.Errorf("rejected under %v, want rule %q", vs, tc.want)
		})
	}
}

// TestManifestPolicyKeepsCurrentProductionShapes is the other half of the table: the
// facts the committed manifest actually carries must all still parse, so tightening a
// rule cannot quietly outlaw a reviewed decision.
func TestManifestPolicyKeepsCurrentProductionShapes(t *testing.T) {
	cases := []struct {
		name string
		edit func(*rawManifest)
	}{
		{"AND expression", func(m *rawManifest) { m.Entries[0].SPDX = "BSD-3-Clause AND MIT" }},
		{"OR expression", func(m *rawManifest) { m.Entries[0].SPDX = "MIT OR Apache-2.0" }},
		{"lower-case operator", func(m *rawManifest) { m.Entries[0].SPDX = "BSD-3-Clause and MIT" }},
		{"custom SQLite LicenseRef", func(m *rawManifest) {
			m.Entries[0].SPDX = "BSD-3-Clause AND LicenseRef-SQLite-Public-Domain"
		}},
		{"pseudo-version", func(m *rawManifest) { m.Entries[0].Version = "v0.0.0-20230129092748-24d4a6f8daec" }},
		{"module@version replacement", func(m *rawManifest) {
			m.Entries[0].Replacement = &rawReplacement{ModulePath: "example.com/y", Version: "v2.0.0"}
		}},
		{"nested text path", func(m *rawManifest) { m.Entries[0].Texts[0].Path = "third_party/LICENSE" }},
		{"unshipped authors list", func(m *rawManifest) {
			m.Entries[0].Texts = append(m.Entries[0].Texts, rawText{Path: "AUTHORS", Role: "authors", Digest: testDigest})
		}},
		{"unshipped informational notice", func(m *rawManifest) {
			m.Entries[0].Texts = append(m.Entries[0].Texts, rawText{Path: "LICENSE-LOGO", Role: "notice", Digest: testDigest})
		}},
		{"all six targets", func(m *rawManifest) { m.Entries[0].Targets = append([]string(nil), releaseTargets...) }},
		{"copyright and review note", func(m *rawManifest) {
			m.Entries[0].Copyright = "Copyright (c) Someone"
			m.Entries[0].ReviewNote = "reviewed 2026-08-05"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := validManifest()
			tc.edit(&raw)
			if _, vs := parseManifest(raw); len(vs) != 0 {
				t.Errorf("a current reviewed shape was rejected: %v", vs)
			}
		})
	}
}

// TestLoadManifestIsStrictAboutTheDocument covers the decode boundary rather than the
// policy: the manifest is hostile input, so exactly one JSON document with no field
// the reader does not account for is the only accepted shape.
func TestLoadManifestIsStrictAboutTheDocument(t *testing.T) {
	const valid = `{"schema_version":1,` +
		`"project_license":{"spdx":"Apache-2.0","path":"LICENSE","digest":"` + testDigest + `"},` +
		`"copyright_holder":"X","entries":[` +
		`{"module_path":"example.com/x","version":"v1.0.0","targets":["linux/amd64"],"spdx":"MIT",` +
		`"texts":[{"path":"LICENSE","role":"license","digest":"` + testDigest + `","must_ship":true}],` +
		`"disposition":"must_appear_in_binary_and_source"}]}`

	write := func(t *testing.T, content string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "licenses.json")
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("accepted", func(t *testing.T) {
		for _, ok := range []string{valid, valid + "\n", valid + "\n  \n\t"} {
			if _, err := loadManifest(write(t, ok)); err != nil {
				t.Errorf("valid single document rejected: %v", err)
			}
		}
	})
	t.Run("trailing data", func(t *testing.T) {
		for _, bad := range []string{valid + "\n{}", valid + " 123", valid + "\ngarbage", valid + valid} {
			if _, err := loadManifest(write(t, bad)); err == nil {
				t.Errorf("trailing data accepted: %q", bad)
			}
		}
	})
	t.Run("unknown fields", func(t *testing.T) {
		// Top level, inside an entry, and inside a text row: strictness must reach every
		// level, or a row could carry side data no reader accounts for.
		for _, bad := range []string{
			strings.Replace(valid, `{"schema_version":1,`, `{"schema_version":1,"extra_inventory":[],`, 1),
			strings.Replace(valid, `{"module_path":"example.com/x",`, `{"module_path":"example.com/x","class":"production",`, 1),
			strings.Replace(valid, `{"path":"LICENSE","role":"license"`, `{"path":"LICENSE","role":"license","optional":true`, 1),
		} {
			if _, err := loadManifest(write(t, bad)); err == nil {
				t.Errorf("unknown field accepted: %q", bad)
			}
		}
	})
}
