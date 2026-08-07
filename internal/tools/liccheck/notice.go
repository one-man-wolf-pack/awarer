package main

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
)

const (
	noticeRuleHeavy = "================================================================================"
	noticeRuleLight = "--------------------------------------------------------------------------------"
)

// renderNotice assembles the deterministic THIRD_PARTY_NOTICES document from the
// reviewed manifest and the pinned source texts. It builds the whole document in
// memory and returns an error rather than partial output if any required text is
// missing or has drifted, so a caller never publishes an incomplete or unreviewed
// notice.
//
// The output is byte-identical across runs with the same source graph: components are
// sorted by module identity, texts by path, only entries with must-ship texts
// contribute, and nothing time-, path-, or host-derived is ever written — only
// manifest fields and verbatim license bytes with line endings normalized to LF.
func renderNotice(m manifest, ev *evidence) ([]byte, error) {
	var buf bytes.Buffer

	buf.WriteString("awa THIRD_PARTY_NOTICES\n")
	buf.WriteString("=======================\n\n")
	buf.WriteString("This file lists the third-party components linked into the awa binary and\n")
	buf.WriteString("reproduces their required license and attribution texts. It is generated from\n")
	buf.WriteString("third_party/licenses.json by `just notices-update`; do not edit it by hand.\n\n")

	// Reference the project license only after verifying the root LICENSE on disk
	// matches the reviewed digest, so notices-update cannot publish a header naming a
	// license whose file was deleted or corrupted.
	pl := m.projectLicense
	plDigest, err := ev.projectLicenseDigest(pl.path)
	if err != nil {
		return nil, fmt.Errorf("notice: %w", err)
	}
	if plDigest.String() != pl.digest.String() {
		return nil, fmt.Errorf("notice: project license %s changed since review (manifest %s, on disk %s)", pl.path, pl.digest, plDigest)
	}
	fmt.Fprintf(&buf, "The awa project itself is licensed under %s; see the %s file.\n", pl.spdx, pl.path)

	entries := append([]entry(nil), m.entries...)
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].module.String() < entries[j].module.String()
	})

	for _, e := range entries {
		texts := shippedTexts(e)
		if len(texts) == 0 {
			continue
		}
		sort.Slice(texts, func(i, j int) bool { return texts[i].relPath < texts[j].relPath })

		buf.WriteString("\n")
		buf.WriteString(noticeRuleHeavy)
		buf.WriteString("\n")
		fmt.Fprintf(&buf, "Component: %s\n", e.module.path)
		if v := e.module.version; v != "" {
			fmt.Fprintf(&buf, "Version:   %s\n", v)
		}
		if repl := e.module.replacement(); repl != "" {
			fmt.Fprintf(&buf, "Replaced:  %s\n", repl)
		}
		fmt.Fprintf(&buf, "License:   %s\n", e.spdx)
		if e.copyright != "" {
			fmt.Fprintf(&buf, "Copyright: %s\n", e.copyright)
		}

		for _, t := range texts {
			// Materialize only reviewed bytes: read the pinned file once and verify
			// that those exact bytes hash to the digest recorded at review time before
			// reproducing them. The single read makes notice generation (and therefore
			// notices-update) fail closed on evidence drift with no read-then-hash
			// window — the published bytes are the hashed bytes.
			raw, got, err := ev.read(e.module, t.relPath)
			if err != nil {
				return nil, fmt.Errorf("notice: reading %s: %w", t.relPath, err)
			}
			if got.String() != t.digest.String() {
				return nil, fmt.Errorf("notice: %s in %s changed since review (manifest %s, pinned %s); re-review and run `just check` before regenerating",
					t.relPath, e.module, t.digest, got)
			}
			buf.WriteString(noticeRuleLight)
			buf.WriteString("\n")
			fmt.Fprintf(&buf, "%s (%s):\n\n", strings.ToUpper(t.role), t.relPath)
			buf.WriteString(normalizeText(raw))
			buf.WriteString("\n")
		}
	}

	return buf.Bytes(), nil
}

// shippedTexts returns the reviewed texts that must be reproduced in the notice, in
// manifest order.
func shippedTexts(e entry) []text {
	var out []text
	for _, t := range e.texts {
		if t.mustShip {
			out = append(out, t)
		}
	}
	return out
}

// normalizeText normalizes only container framing: CRLF line endings become LF and
// trailing blank lines are trimmed to a single terminating newline. License wording
// bytes are never altered.
func normalizeText(raw []byte) string {
	s := strings.ReplaceAll(string(raw), "\r\n", "\n")
	s = strings.TrimRight(s, "\n")
	return s + "\n"
}
