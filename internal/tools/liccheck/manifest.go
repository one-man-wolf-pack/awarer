package main

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"awarer/internal/domain/hashing"
)

// manifestSchemaVersion is the committed manifest's schema version. A future
// incompatible shape change bumps this so an old reader rejects a new file rather
// than misreading it.
const manifestSchemaVersion = 1

// The rules the audit can reject a manifest under. Each names one concrete check, so
// a diagnostic points at the policy decision that needs attention and a test asserts
// the rule rather than matching prose.
const (
	ruleUnmanifested        = "unmanifested"         // an observed production module has no manifest entry
	ruleStaleEntry          = "stale_entry"          // a manifest entry is selected by no release target
	ruleVersionDrift        = "version_drift"        // an observed version or replacement differs from the manifest
	ruleTargetDrift         = "target_drift"         // an entry's declared targets differ from those that select it
	ruleUnresolvedModule    = "unresolved_module"    // a reviewed module's exact identity is not in the local cache
	ruleMissingText         = "missing_text"         // a reviewed license file is gone from the pinned module
	ruleTextChanged         = "text_changed"         // a reviewed license file's bytes no longer hash to the manifest digest
	ruleUnapprovedSPDX      = "unapproved_spdx"      // an SPDX expression is not built from approved atoms
	ruleDuplicateEntry      = "duplicate_entry"      // two entries share a module path
	ruleMissingDisposition  = "missing_disposition"  // an entry omits or malforms its distribution disposition
	ruleDispositionMismatch = "disposition_mismatch" // disposition contradicts the per-text must_ship flags
	ruleUnshippedLicense    = "unshipped_license"    // a compliance-critical text is marked must_ship=false
	ruleMalformedEntry      = "malformed_entry"      // an entry field is otherwise malformed
	ruleManifestHeader      = "manifest_header"      // a manifest-level field is invalid
	ruleProjectLicense      = "project_license"      // the project's own license record is malformed or drifted
)

// violation is a single audit failure, carrying enough context to answer what
// changed, where the evidence lives, and which reviewed artifact must be updated.
type violation struct {
	rule   string
	module string
	file   string
	detail string
}

// String renders a single-line diagnostic.
func (v violation) String() string {
	s := "[" + v.rule + "]"
	if v.module != "" {
		s += " module=" + v.module
	}
	if v.file != "" {
		s += " file=" + v.file
	}
	if v.detail != "" {
		s += ": " + v.detail
	}
	return s
}

// sortViolations orders violations deterministically by rule, module, then file so a
// report and its tests are stable.
func sortViolations(vs []violation) {
	sort.Slice(vs, func(i, j int) bool {
		a, b := vs[i], vs[j]
		if a.rule != b.rule {
			return a.rule < b.rule
		}
		if a.module != b.module {
			return a.module < b.module
		}
		return a.file < b.file
	})
}

// The on-disk JSON shape of the reviewed license manifest. It is production-only:
// the manifest describes exactly the modules linked into a shipped awa binary, and
// nothing else. Test-only and build-tool dependencies are not release inventory —
// they ship in no archive and generate no notice — so they are reviewed as ordinary
// dependencies and pins rather than tracked here.
//
// Decoding is strict (DisallowUnknownFields plus a single-document check in
// loadManifest), so a row cannot carry side data no reader accounts for.
type rawManifest struct {
	SchemaVersion   int               `json:"schema_version"`
	ProjectLicense  rawProjectLicense `json:"project_license"`
	CopyrightHolder string            `json:"copyright_holder"`
	Entries         []rawEntry        `json:"entries"`
}

// rawProjectLicense is the JSON shape of the project's own license record.
type rawProjectLicense struct {
	SPDX   string `json:"spdx"`
	Path   string `json:"path"`
	Digest string `json:"digest"`
}

// rawEntry is the JSON shape of one reviewed production row.
type rawEntry struct {
	ModulePath  string          `json:"module_path"`
	Version     string          `json:"version,omitempty"`
	Replacement *rawReplacement `json:"replacement,omitempty"`
	Targets     []string        `json:"targets,omitempty"`
	SPDX        string          `json:"spdx"`
	Texts       []rawText       `json:"texts"`
	Disposition string          `json:"disposition"`
	Copyright   string          `json:"copyright,omitempty"`
	ReviewNote  string          `json:"review_note,omitempty"`
}

// rawText is the JSON shape of one reviewed license/attribution text.
type rawText struct {
	Path     string `json:"path"`
	Role     string `json:"role"`
	Digest   string `json:"digest"`
	MustShip bool   `json:"must_ship"`
}

// rawReplacement is the JSON shape of a module@version replacement. Local
// filesystem replacements are not representable: they are host-dependent and not
// reproducible, so the audit rejects them rather than recording one.
type rawReplacement struct {
	ModulePath string `json:"module_path"`
	Version    string `json:"version"`
}

// moduleID identifies a Go module selection: its path, the version Go resolved, and
// any module@version replacement Go applied. Equality covers every field, so a
// version bump or a newly introduced replacement is drift rather than a silent
// match, and the string form is what keys the evidence index — a manifest naming
// another version simply will not find the selected version's directory.
type moduleID struct {
	path        string
	version     string
	replacePath string
	replaceVer  string
}

// String renders "path@version" (or "path@version=>replacement@version" when
// replaced), stable enough to key maps and sort deterministically.
func (m moduleID) String() string {
	s := m.path
	if m.version != "" {
		s += "@" + m.version
	}
	if r := m.replacement(); r != "" {
		s += "=>" + r
	}
	return s
}

// replacement renders the replacement target, or "" when unreplaced.
func (m moduleID) replacement() string {
	if m.replacePath == "" {
		return ""
	}
	return m.replacePath + "@" + m.replaceVer
}

// newModuleID builds an unreplaced identity, rejecting a blank or non-Go version so
// a malformed selection cannot enter the graph or the manifest.
func newModuleID(modPath, version string) (moduleID, error) {
	if modPath == "" {
		return moduleID{}, fmt.Errorf("invalid module identity: empty path")
	}
	if version == "" {
		return moduleID{}, fmt.Errorf("invalid module identity for %q: empty version", modPath)
	}
	if !strings.HasPrefix(version, "v") {
		return moduleID{}, fmt.Errorf("invalid module identity for %q: version %q must start with \"v\"", modPath, version)
	}
	return moduleID{path: modPath, version: version}, nil
}

// withReplacement returns a copy replaced by another module@version. A
// module@version replacement is reproducible and is the only kind the audit
// accepts; a local (versionless) replacement must be rejected by the caller.
func (m moduleID) withReplacement(replacePath, replaceVer string) (moduleID, error) {
	if replacePath == "" || replaceVer == "" {
		return moduleID{}, fmt.Errorf("invalid module replacement for %q: replacement path and version required", m.path)
	}
	if !strings.HasPrefix(replaceVer, "v") {
		return moduleID{}, fmt.Errorf("invalid module replacement for %q: version %q must start with \"v\"", m.path, replaceVer)
	}
	m.replacePath = replacePath
	m.replaceVer = replaceVer
	return m, nil
}

// approvedSPDXAtoms is the closed allow-list of SPDX license identifiers the project
// has reviewed and accepts for a shipped component. An expression is built only from
// these atoms combined with AND / OR; anything else fails, so an unreviewed license
// cannot enter the production audit.
//
// LicenseRef-SQLite-Public-Domain is a deliberate custom SPDX LicenseRef for the
// SQLite public-domain dedication carried by modernc.org/sqlite's LICENSE-SQLITE
// file: SQLite's own code is dedicated to the public domain, which SPDX has no
// standard short identifier for, so the reviewed decision records it explicitly.
var approvedSPDXAtoms = map[string]struct{}{
	"MIT":                             {},
	"BSD-2-Clause":                    {},
	"BSD-3-Clause":                    {},
	"Apache-2.0":                      {},
	"ISC":                             {},
	"CC0-1.0":                         {},
	"LicenseRef-SQLite-Public-Domain": {},
}

// parseSPDX validates an SPDX expression against the approved atoms. Tokens
// alternate atom, operator, atom, ... starting and ending with an atom (even indices
// are atoms, odd are operators), and operators are normalized to upper case.
// Parentheses and the WITH exception operator are intentionally unsupported: the
// current graph needs neither, and a small grammar keeps the allow-list auditable.
func parseSPDX(s string) (string, error) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return "", fmt.Errorf("invalid license expression %q: empty", s)
	}
	normalized := make([]string, len(fields))
	for i, tok := range fields {
		if i%2 == 0 {
			if _, ok := approvedSPDXAtoms[tok]; !ok {
				return "", fmt.Errorf("invalid license expression %q: unapproved license %q", s, tok)
			}
			normalized[i] = tok
			continue
		}
		op := strings.ToUpper(tok)
		if op != "AND" && op != "OR" {
			return "", fmt.Errorf("invalid license expression %q: unexpected operator %q (want AND or OR)", s, tok)
		}
		normalized[i] = op
	}
	if len(fields)%2 == 0 {
		return "", fmt.Errorf("invalid license expression %q: trailing operator", s)
	}
	return strings.Join(normalized, " "), nil
}

// textRoles are the reviewed roles a license/attribution file may carry, mapped to
// whether the role makes it a compliance-critical attribution that must be
// reproduced in the notice. License, patent-grant, and bundled third-party notices
// carry redistribution obligations; authors lists and informational notices do not.
var textRoles = map[string]bool{
	"license":     true,
	"patents":     true,
	"third_party": true,
	"notice":      false,
	"authors":     false,
}

// The two reviewed redistribution obligations. There is no "unset" member: an entry
// that fails to declare one is a detectable manifest error rather than a silent
// default. A separate source-only disposition is intentionally absent — awa ships a
// single THIRD_PARTY_NOTICES included in every archive, so there is no source-only
// planner to honor it.
const (
	dispositionBinaryAndSource  = "must_appear_in_binary_and_source"
	dispositionNoRedistribution = "no_redistribution_text"
)

// text is one validated reviewed license/attribution file inside a pinned module.
// The path is always module-relative (never absolute, never escaping the module
// root) so no module-cache path, username, or host state can leak into the committed
// manifest or the generated notice.
type text struct {
	relPath  string
	role     string
	digest   hashing.TreeHash
	mustShip bool
}

// entry is one validated reviewed production row: a module linked into a shipped awa
// binary, the release targets that select it, the reviewed SPDX expression, the
// reviewed texts with their digests, the redistribution disposition, and an optional
// required copyright.
type entry struct {
	module    moduleID
	targets   []string
	spdx      string
	texts     []text
	copyright string
}

// projectLicense is the reviewed identity of the project's own root LICENSE file, so
// it is first-class reviewed evidence like any third-party text: the gate hashes the
// file on disk and compares it here, and the LICENSE cannot be deleted, replaced, or
// corrupted while the gate keeps asserting it.
type projectLicense struct {
	spdx   string
	path   string
	digest hashing.TreeHash
}

// manifest is the validated reviewed license policy. It exists only when
// parseManifest reported no violation, so every entry it holds is a complete,
// consistent decision and the notice generator can rely on that.
type manifest struct {
	projectLicense projectLicense
	entries        []entry
	// declared is every module path the file listed, including rows that were
	// rejected and therefore left out of entries. It exists so the graph check can
	// tell "no reviewed row for this module" from "the row is there but malformed",
	// and report the first as unmanifested without contradicting the second.
	declared map[string]struct{}
}

// parseManifest validates a decoded manifest in one strict pass, returning the rows
// that parsed cleanly and every policy violation it found.
//
// It reports every violation rather than stopping at the first because the common
// failure is a dependency bump that touches several rows at once; failing fast would
// charge a whole gate run per row. Callers must treat any violation as fatal — the
// returned manifest is complete only when the violation slice is empty.
func parseManifest(raw rawManifest) (manifest, []violation) {
	var vs []violation

	if raw.SchemaVersion != manifestSchemaVersion {
		vs = append(vs, violation{rule: ruleManifestHeader, detail: fmt.Sprintf("schema_version is %d, want %d", raw.SchemaVersion, manifestSchemaVersion)})
	}
	if raw.CopyrightHolder == "" {
		vs = append(vs, violation{rule: ruleManifestHeader, detail: "copyright_holder must be set"})
	}

	pl, err := parseProjectLicense(raw.ProjectLicense)
	if err != nil {
		vs = append(vs, violation{rule: ruleProjectLicense, file: raw.ProjectLicense.Path, detail: err.Error()})
	}

	entries := make([]entry, 0, len(raw.Entries))
	declared := make(map[string]struct{}, len(raw.Entries))
	for _, re := range raw.Entries {
		if _, dup := declared[re.ModulePath]; dup {
			vs = append(vs, violation{rule: ruleDuplicateEntry, module: re.ModulePath, detail: "module appears more than once in the manifest"})
			continue
		}
		declared[re.ModulePath] = struct{}{}
		e, evs := parseEntry(re)
		vs = append(vs, evs...)
		if len(evs) == 0 {
			entries = append(entries, e)
		}
	}

	sortViolations(vs)
	return manifest{projectLicense: pl, entries: entries, declared: declared}, vs
}

// parseProjectLicense validates the project's own license record. Its SPDX is gated
// against the shipped allow-list, because the project ships under it.
func parseProjectLicense(raw rawProjectLicense) (projectLicense, error) {
	spdx, err := parseSPDX(raw.SPDX)
	if err != nil {
		return projectLicense{}, fmt.Errorf("project license spdx: %w", err)
	}
	if err := validRelPath(raw.Path); err != nil {
		return projectLicense{}, fmt.Errorf("project license: %w", err)
	}
	digest, err := hashing.ParseTreeHash(raw.Digest)
	if err != nil {
		return projectLicense{}, fmt.Errorf("project license digest: %w", err)
	}
	return projectLicense{spdx: spdx, path: raw.Path, digest: digest}, nil
}

// parseEntry validates every field of one raw row, returning the validated entry and
// each violation found. An entry is used downstream only when it produced none, so
// an incomplete or contradictory decision never reaches the graph check or notice.
func parseEntry(re rawEntry) (entry, []violation) {
	var vs []violation
	e := entry{copyright: re.Copyright}

	module, err := rawEntryIdentity(re)
	if err != nil {
		vs = append(vs, violation{rule: ruleMalformedEntry, module: re.ModulePath, detail: err.Error()})
	}
	e.module = module

	// A production module must name the targets that select it: an entry claiming no
	// target claims no reachability, which full-scope parity could then never
	// contradict. This is checked in both scopes, so a fast gate still rejects it.
	if len(re.Targets) == 0 {
		vs = append(vs, violation{rule: ruleMalformedEntry, module: re.ModulePath, detail: "a production entry must name the targets that select it"})
	}
	seenTargets := make(map[string]struct{}, len(re.Targets))
	for _, ts := range re.Targets {
		if err := validTarget(ts); err != nil {
			vs = append(vs, violation{rule: ruleMalformedEntry, module: re.ModulePath, detail: err.Error()})
			continue
		}
		// A repeated target is rejected here rather than left to full-scope parity: the
		// set comparison there would report it as target drift, which sends a reviewer
		// looking for a graph change that never happened.
		if _, dup := seenTargets[ts]; dup {
			vs = append(vs, violation{rule: ruleMalformedEntry, module: re.ModulePath, detail: fmt.Sprintf("target %q is named more than once", ts)})
			continue
		}
		seenTargets[ts] = struct{}{}
		e.targets = append(e.targets, ts)
	}
	sort.Strings(e.targets)

	spdx, err := parseSPDX(re.SPDX)
	if err != nil {
		vs = append(vs, violation{rule: ruleUnapprovedSPDX, module: re.ModulePath, detail: err.Error()})
	}
	e.spdx = spdx

	if len(re.Texts) == 0 {
		vs = append(vs, violation{rule: ruleMalformedEntry, module: re.ModulePath, detail: "at least one reviewed license text is required"})
	}
	// Counted from what the row declares, not from the rows that parsed: a text with a
	// malformed digest still states an intent to ship, and deriving the count from
	// parsed texts would report the entry as shipping nothing on top of the real defect.
	shipped := 0
	for _, rt := range re.Texts {
		if rt.MustShip {
			shipped++
		}
	}
	for _, rt := range re.Texts {
		t, err := parseText(rt)
		if err != nil {
			vs = append(vs, violation{rule: ruleMalformedEntry, module: re.ModulePath, file: rt.Path, detail: err.Error()})
			continue
		}
		if textRoles[t.role] && !t.mustShip {
			vs = append(vs, violation{rule: ruleUnshippedLicense, module: re.ModulePath, file: rt.Path,
				detail: "a " + t.role + " text must be shipped in the notice (set must_ship=true)"})
		}
		e.texts = append(e.texts, t)
	}

	// The disposition must agree with the per-text must_ship flags, so an entry can
	// never claim it ships while shipping nothing — which would silently drop a whole
	// component from the notice — or claim no redistribution while marking a text to
	// ship.
	switch re.Disposition {
	case dispositionNoRedistribution:
		if shipped > 0 {
			vs = append(vs, violation{rule: ruleDispositionMismatch, module: re.ModulePath,
				detail: "disposition " + dispositionNoRedistribution + " but a text is marked must_ship=true"})
		}
	case dispositionBinaryAndSource:
		if shipped == 0 {
			vs = append(vs, violation{rule: ruleDispositionMismatch, module: re.ModulePath,
				detail: "disposition " + dispositionBinaryAndSource + " requires at least one text marked must_ship=true"})
		}
	default:
		vs = append(vs, violation{rule: ruleMissingDisposition, module: re.ModulePath,
			detail: fmt.Sprintf("invalid distribution disposition %q: want %s or %s", re.Disposition, dispositionBinaryAndSource, dispositionNoRedistribution)})
	}

	return e, vs
}

// rawEntryIdentity resolves a raw row's module identity, applying any module@version
// replacement. A malformed replacement — missing path or version, as a local
// replacement would be — is rejected.
func rawEntryIdentity(re rawEntry) (moduleID, error) {
	id, err := newModuleID(re.ModulePath, re.Version)
	if err != nil {
		return moduleID{}, err
	}
	if re.Replacement == nil {
		return id, nil
	}
	return id.withReplacement(re.Replacement.ModulePath, re.Replacement.Version)
}

// parseText validates one reviewed text row.
func parseText(rt rawText) (text, error) {
	if err := validRelPath(rt.Path); err != nil {
		return text{}, err
	}
	if _, ok := textRoles[rt.Role]; !ok {
		return text{}, fmt.Errorf("invalid text role %q: want license, notice, authors, patents, or third_party", rt.Role)
	}
	digest, err := hashing.ParseTreeHash(rt.Digest)
	if err != nil {
		return text{}, err
	}
	return text{relPath: rt.Path, role: rt.Role, digest: digest, mustShip: rt.MustShip}, nil
}

// validRelPath rejects an empty, absolute, or parent-escaping relative path, so no
// module-cache path, repo-escaping path, or host state can enter a reviewed value.
func validRelPath(relPath string) error {
	if relPath == "" {
		return fmt.Errorf("empty path")
	}
	if path.IsAbs(relPath) || strings.HasPrefix(relPath, "/") || strings.HasPrefix(relPath, "\\") {
		return fmt.Errorf("path %q must be relative, not absolute", relPath)
	}
	clean := path.Clean(relPath)
	if clean == ".." || strings.HasPrefix(clean, "../") || clean != relPath {
		return fmt.Errorf("path %q must be clean and stay within the root", relPath)
	}
	return nil
}

// validTarget checks a "goos/goarch" pair. Both halves must be non-empty and free of
// slashes and spaces so the string form round-trips unambiguously.
func validTarget(s string) error {
	goos, goarch, found := strings.Cut(s, "/")
	if !found {
		return fmt.Errorf("invalid release target %q: want \"goos/goarch\"", s)
	}
	if goos == "" || goarch == "" {
		return fmt.Errorf("invalid release target %q: empty goos or goarch", s)
	}
	if strings.ContainsAny(goos, "/ ") || strings.ContainsAny(goarch, "/ ") {
		return fmt.Errorf("invalid release target %q: goos/goarch must not contain spaces or extra slashes", s)
	}
	return nil
}
