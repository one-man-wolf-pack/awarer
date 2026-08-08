package cli

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"awarer/internal/app/help"
	domainconfig "awarer/internal/domain/config"
	"awarer/internal/domain/configref"
	"awarer/internal/domain/docbundle"
	"awarer/internal/domain/doctor"
	"awarer/internal/domain/gc"
	"awarer/internal/domain/runcache"
)

// This file is the documentation coverage matrix: the checked mapping from "what awa
// released" to "which documentation page answers for it".
//
// It lives in package cli because this is the only package that can see both
// halves — the command registry, capability catalog, and exit-code catalog on one
// side, and the help catalog on the other. The help package cannot import the CLI,
// and the CLI's own drift audit (docsbundle_test.go) proves the generated reference
// is exhaustive but says nothing about whether a human ever explained the thing.
//
// The keys are derived MECHANICALLY from the live registries, never listed by hand.
// That is what makes the matrix load-bearing: adding a command, a global option, an
// exit code, a config section, or a cache-miss reason produces a key with no owner
// and fails here by name, so shipping an undocumented capability takes a deliberate
// edit to this file rather than silence.

// coverageOwner names the topic that owns a released capability, or records an
// explicit exemption with the reason it needs none.
type coverageOwner struct {
	topic  string // owning topic slug; "" means exempt
	reason string // required when topic is ""
}

// docCoverage maps every released capability to the ONE authored topic that must
// answer for it. "Must answer" is enforced, not asserted: the owning topic's body
// has to actually contain the invocation, code, section, token, or flag spelling —
// see coverageEvidence below. The owner must be authored; see authoredTopicBodies
// for why a generated page cannot own a row.
//
// One owner per capability is deliberate. Several pages may mention a command; the
// point of the matrix is to name the page that is wrong when the documentation is
// wrong, so that a probe failure has an address.
var docCoverage = map[string]coverageOwner{
	// --- commands -------------------------------------------------------------
	"command:init":       {topic: "quickstart"},
	"command:status":     {topic: "status"},
	"command:checkpoint": {topic: "checkpoints"},
	"command:log":        {topic: "checkpoints"},
	"command:changes":    {topic: "diff"},
	"command:diff":       {topic: "diff"},
	"command:restore":    {topic: "restore"},
	"command:run":        {topic: "run"},
	"command:gc":         {topic: "gc"},
	"command:doctor":     {topic: "doctor"},
	"command:config":     {topic: "quickstart"},
	"command:state":      {topic: "integrations"},
	"command:docs":       {topic: "agents"},
	"command:version":    {topic: "install"},
	"command:help": {reason: "help is the retrieval surface itself; " +
		"'awa help topics' is its own index and every topic page is its output"},

	// --- subcommands ----------------------------------------------------------
	"command:run ls":           {topic: "run"},
	"command:run explain":      {topic: "run"},
	"command:run log":          {topic: "inspect"},
	"command:run show":         {topic: "inspect"},
	"command:run rm":           {topic: "inspect"},
	"command:config path":      {topic: "troubleshooting"},
	"command:config show":      {topic: "quickstart"},
	"command:config effective": {topic: "quickstart"},
	"command:config validate":  {topic: "quickstart"},
	"command:config template":  {topic: "quickstart"},
	"command:config init":      {topic: "quickstart"},
	"command:state resolve":    {topic: "integrations"},
	"command:state compare":    {topic: "integrations"},
	"command:docs export":      {topic: "agents"},

	// --- global options -------------------------------------------------------
	"global:--root":                {topic: "quickstart"},
	"global:--config":              {topic: "quickstart"},
	"global:--json":                {topic: "json"},
	"global:--trust-mode/--strict": {topic: "run"},

	// --- exit codes -----------------------------------------------------------
	"exit:0":   {topic: "exit-codes"},
	"exit:1":   {topic: "exit-codes"},
	"exit:2":   {topic: "exit-codes"},
	"exit:3":   {topic: "exit-codes"},
	"exit:4":   {topic: "exit-codes"},
	"exit:5":   {topic: "exit-codes"},
	"exit:6":   {topic: "exit-codes"},
	"exit:130": {topic: "exit-codes"},

	// --- configuration sections ------------------------------------------------
	// The generated configuration reference documents every key. These rows require
	// something it cannot give: that the operational page whose behavior a section
	// tunes names that section, so an agent reading about the behavior learns which
	// table to edit without first having to guess the section name.
	"config:scope":      {topic: "ignores"},
	"config:history":    {topic: "ignores"},
	"config:hashing":    {topic: "run"},
	"config:checkpoint": {topic: "checkpoints"},
	"config:run":        {topic: "run"},
	"config:gc":         {topic: "gc"},
	"config:diff":       {topic: "diff"},
	"config:locks":      {topic: "gc"},
	"config:ui":         {topic: "inspect"},

	// --- cache-miss reason vocabulary -------------------------------------------
	// run.md owns the whole closed vocabulary; troubleshooting routes to it by
	// symptom rather than restating it.
	"reason:input-tree-differs":       {topic: "run"},
	"reason:mutated-state":            {topic: "run"},
	"reason:effect-state-differs":     {topic: "run"},
	"reason:effect-state-unavailable": {topic: "run"},
	"reason:fast-trust-mode":          {topic: "run"},
	"reason:expired":                  {topic: "run"},
	"reason:env-mismatch":             {topic: "run"},
	"reason:cwd-mismatch":             {topic: "run"},
	"reason:config-mismatch":          {topic: "run"},
	"reason:platform-mismatch":        {topic: "run"},
	"reason:payload-missing":          {topic: "run"},
	"reason:corrupt":                  {topic: "run"},
	"reason:post-scan-failed":         {topic: "run"},
	"reason:non-cacheable-policy":     {topic: "run"},
	"reason:record-only":              {topic: "run"},
	"reason:no-cache":                 {topic: "run"},
	"reason:stdin-not-keyed":          {topic: "run"},
	"reason:skipped-inputs":           {topic: "run"},
	"reason:capture-disabled":         {topic: "run"},
	"reason:failed-not-cached":        {topic: "run"},
}

// requiredCoverageKeys derives the capability keys from the live registries. It is
// the mechanical half of the matrix: nothing here is a literal list of what awa
// happens to ship today.
func requiredCoverageKeys() []string {
	var keys []string
	for _, c := range newRouter().commands {
		keys = append(keys, "command:"+c.name)
		for _, sc := range c.subcommands {
			keys = append(keys, "command:"+c.name+" "+sc.name)
		}
	}
	for _, cap := range allCapabilities {
		keys = append(keys, "global:"+string(cap))
	}
	for _, code := range exitCodes() {
		keys = append(keys, "exit:"+strconv.Itoa(int(code)))
	}
	for _, s := range configref.Sections() {
		keys = append(keys, "config:"+s.Name)
	}
	for _, r := range runcache.MismatchReasons() {
		keys = append(keys, "reason:"+r.String())
	}
	return keys
}

// TestEveryReleasedCapabilityHasAnOwningTopic is the coverage matrix itself.
//
// An empty or partial registry walk needs no separate floor: the reverse pass below
// requires every docCoverage row to name a derived key, so a walk that stopped early
// fails by naming each row it no longer reaches.
func TestEveryReleasedCapabilityHasAnOwningTopic(t *testing.T) {
	required := requiredCoverageKeys()
	bodies := authoredTopicBodies(t)
	seen := make(map[string]bool, len(required))

	for _, key := range required {
		if seen[key] {
			t.Errorf("capability key %q derived twice; the key scheme is ambiguous", key)
			continue
		}
		seen[key] = true

		owner, ok := docCoverage[key]
		if !ok {
			t.Errorf("released capability %q has no owning documentation topic; "+
				"add a row to docCoverage naming the page that answers for it, or a justified exemption", key)
			continue
		}
		if owner.topic == "" {
			// An exemption is an explicit reviewed row, so it has to say something;
			// how much it says is the reviewer's judgement, not this test's.
			if owner.reason == "" {
				t.Errorf("capability %q is exempt with no reason; state why no page owns it", key)
			}
			continue
		}
		body, ok := bodies[owner.topic]
		if !ok {
			t.Errorf("capability %q names owning topic %q, which is not an authored topic; "+
				"a generated page cannot own a row, because it is rendered from the same "+
				"registry the key comes from", key, owner.topic)
			continue
		}
		if want, ok := coverageEvidence(key, body); !ok {
			t.Errorf("capability %q is assigned to topic %q, but that page never mentions %q; "+
				"either document it there or move the row", key, owner.topic, want)
		}
	}

	for key := range docCoverage {
		if !seen[key] {
			t.Errorf("docCoverage row %q names no released capability; remove it or fix the key", key)
		}
	}
}

// coverageEvidence reports whether body earns the coverage row, and what was looked
// for when it does not. The evidence differs by key kind because "documented" means
// something different for a command than for an exit code.
//
// Matching goes through searchable() so that "awa config effective" stays found when
// the sentence around it is rewrapped: rewrapping is a formatting change, not the
// removal of a documented capability. The exit-code branch is the exception — it reads
// table rows with a line-anchored scanner, which collapsing newlines would defeat.
func coverageEvidence(key, body string) (string, bool) {
	kind, value, _ := strings.Cut(key, ":")
	text := searchable(body)
	contains := func(want string) bool { return strings.Contains(text, searchable(want)) }
	switch kind {
	case "command":
		want := "awa " + value
		return want, contains(want)
	case "global":
		// A capability may have several spellings; naming any one of them is
		// enough for a page to have introduced the option.
		for _, s := range capabilityInfo[capability(value)].spellings {
			if contains(s.flag) {
				return value, true
			}
		}
		return value, false
	case "exit":
		return value, exitCodeTableEntries(body)[value]
	case "config":
		want := "[" + value + "]"
		return want, contains(want)
	case "reason":
		return value, contains(value)
	}
	return key, false
}

// authoredTopicBodies indexes the topic bodies a human wrote, by slug. Generated
// pages are deliberately absent, and that exclusion is what keeps this file's
// assertions from being circular: the configuration reference is rendered FROM
// configref.Sections(), the same registry the "config:" keys are derived from, so
// naming it as an owner would let a new section supply both the key and the
// evidence that it is documented. The generated corpus is proven exhaustive by
// docsbundle_test.go; what cannot be proven mechanically, and is therefore this
// file's whole subject, is whether a human explained the thing.
func authoredTopicBodies(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, topic := range help.Topics() {
		if topic.Kind != docbundle.KindTopic {
			continue
		}
		out[topic.Slug.String()] = topic.Body
	}
	if len(out) == 0 {
		t.Fatal("the help catalog has no authored topics; the lookup looks wrong")
	}
	return out
}

// searchable normalizes a body for token matching: whitespace runs collapse to one
// space and case is folded. Both matter. Documentation is hard-wrapped, so a token
// would otherwise go missing the moment a sentence is rewrapped — which is a
// formatting change, not a change to what the page covers. Folding case keeps the
// match about what a page says rather than about how it capitalized it mid-sentence.
func searchable(body string) string {
	return strings.ToLower(strings.Join(strings.Fields(body), " "))
}

// forbiddenClaims are phrases no published document may contain. They fall into two
// groups: promises awa cannot keep, and behavior that does not exist in this
// release. Documenting unreleased behavior is the more dangerous of the two, because
// an agent will try to run it.
//
// Each phrase is matched on word boundaries and with whitespace-tolerant gaps, not as
// a bare substring. The boundaries keep a banned phrase from firing inside a longer
// word, and the gaps keep a hard-wrapped sentence from hiding one: the corpus wraps at
// column width, so "always\nreusable" is the same claim as "always reusable".
var forbiddenClaims = []struct {
	phrase string
	why    string
}{
	{"equals the git diff", "a checkpoint delta is not the uncommitted git diff"},
	{"safely restored", "ignored and unobserved paths cannot be restored"},
	{"always reusable", "reuse is conditional on observed inputs and configured policy"},
	{"guaranteed to hit", "the cache prefers a false miss; a hit is never guaranteed"},
	{"proves the command is correct", "a cache hit is reuse for observed inputs, not a correctness proof"},
	// Case-only collisions on a case-insensitive filesystem are best-effort: awa
	// implements no detection for them. Each spelling below is affirmative-only —
	// naming the actor, or using the passive without a negator — so no negation of it
	// can match. That is the constraint every entry in this list has to meet: the
	// honest sentence about a thing awa does not do is the negation of the dishonest
	// one, so "detects case" would fail a page for correctly writing "awa never
	// detects case-only collisions", and "collision detection" would fail one for
	// writing that awa has none.
	{"awa detects case", "awa does not detect case-only collisions; the behavior is best-effort"},
	{"case collisions are detected", "awa does not detect case-only collisions; the behavior is best-effort"},
	{"awa detects path collisions", "awa does not detect path collisions; the behavior is best-effort"},
}

// forbiddenClaimRes compiles each forbidden phrase into a whitespace-tolerant,
// word-bounded pattern, so hard wrapping cannot hide a claim and an ordinary word
// that merely starts with one cannot raise a false alarm.
var forbiddenClaimRes = func() []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(forbiddenClaims))
	for i, fc := range forbiddenClaims {
		out[i] = regexp.MustCompile(`(?i)\b` + strings.Join(strings.Fields(regexp.QuoteMeta(fc.phrase)), `\s+`) + `\b`)
	}
	return out
}()

// TestNoDocumentMakesAForbiddenClaim scans the whole published corpus — authored
// topics AND generated reference pages — because a forbidden claim can be
// introduced by a help string or a summary just as easily as by a topic body.
func TestNoDocumentMakesAForbiddenClaim(t *testing.T) {
	docs := mustBundle(t).Documents()
	if len(docs) == 0 {
		t.Fatal("the bundle publishes no documents; the scan looks vacuous")
	}
	for _, d := range docs {
		body := string(d.Body())
		for i, re := range forbiddenClaimRes {
			if m := re.FindString(body); m != "" {
				t.Errorf("document %q contains the forbidden phrase %q: %s",
					d.Slug(), m, forbiddenClaims[i].why)
			}
		}
	}
}

// reviewedDoctorCodePrefixes are the grouped spellings doctor.md may use to cover a
// whole family of finding codes at once, each with the reason the family is clearer as
// one row than as four. A prefix is honored ONLY if it appears here: the page cannot
// widen its own coverage by writing a broader wildcard, because doing so also has to
// pass through this review gate.
//
// This is why doctor codes get their own guard rather than one row per code in
// docCoverage. The cache-miss reasons are documented one token per line, so
// exact-token evidence fits them; the finding codes are documented by subsystem
// family, and forcing them into exact-token rows would mean either a line per code
// or a wildcard nobody reviewed. (The exact count is pinned in the doctor domain's
// own test, so it is deliberately not restated here.)
var reviewedDoctorCodePrefixes = map[string]string{
	"required-dir-":     "missing and not-a-directory are the same finding about the same path",
	"checkpoint-":       "five ways one checkpoint record can be unreadable; the reader acts on all of them the same way",
	"run-":              "four ways one stored run can be unreadable",
	"restore-recovery-": "the two ways a restore's undo evidence stops being usable: the record itself, or the content it captured",
	"index-":            "three ways the worktree index is unusable and must be rebuilt",
	"state-gitignore-":  "three states of the one awa-owned guard file",
	"nested-":           "a marker below this root, or a scan that could not rule one out",
	"ancestor-":         "a marker above this root; paired with nested- in the same row",
}

// minDoctorCodePrefixLen is the shortest string that can still be a subsystem name
// plus its separating hyphen — "run-" is the shortest real one. Combined with the
// must-end-at-a-hyphen rule this stops a prefix from degenerating into a blanket:
// the reviewed allowlist is the actual gate, and these two checks are what keep a
// careless edit to the allowlist from opening one.
const minDoctorCodePrefixLen = len("run-")

// doctorCodeTokenRe is one finding-code spelling, or a family wildcard. It matches a
// whole token, never a prefix of a longer string, so the extraction below decides what
// counts as a token position and this decides only whether the text is a code.
//
// Splitting those two jobs is deliberate. Encoding the row shape into the pattern is
// what an earlier version did — it recognized "a" and "a, b" and therefore silently
// stopped seeing the third code in a three-code row, reporting a documented code as
// undocumented.
var doctorCodeTokenRe = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*-?\*?$`)

// doctorCodeColumns returns the code spellings in DEFINITION position in an authored
// body: inside a fenced block, unindented, in the column before the gap that separates
// a code from its description, comma-separated when a row names a family together.
//
// Definition position is what makes a hit evidence rather than coincidence. The word
// "run" in a sentence, or a code named inside a row's wrapped description, must not be
// able to satisfy that code's coverage — a prose-only match is exactly the false pass
// this check exists to reject, and it is the one an earlier version let through.
func doctorCodeColumns(body string) []string {
	var out []string
	for _, line := range fencedLines(body) {
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			continue // an indented continuation line is a wrapped description
		}
		column, _, _ := strings.Cut(line, "  ")
		for _, tok := range strings.Split(column, ",") {
			if tok = strings.TrimSpace(tok); doctorCodeTokenRe.MatchString(tok) {
				out = append(out, tok)
			}
		}
	}
	return out
}

// TestEveryDoctorFindingCodeIsDocumented holds the doctor diagnosis vocabulary to the
// same standard as the cache-miss vocabulary: a closed set of stable machine tokens
// that a script is invited to branch on must be reachable from the page that owns it.
//
// A new code in internal/domain/doctor fails here by name until doctor.md either names
// it or covers it with a reviewed family prefix, so shipping a token no script can
// learn about takes a deliberate edit rather than silence.
func TestEveryDoctorFindingCodeIsDocumented(t *testing.T) {
	body, ok := authoredTopicBodies(t)["doctor"]
	if !ok {
		t.Fatal("the doctor topic is not in the help catalog")
	}

	exact := map[string]bool{}
	prefixes := map[string]bool{}
	for _, tok := range doctorCodeColumns(body) {
		if prefix, isFamily := strings.CutSuffix(tok, "*"); isFamily {
			prefixes[prefix] = true
		} else {
			exact[tok] = true
		}
	}

	for prefix := range reviewedDoctorCodePrefixes {
		if !prefixes[prefix] {
			t.Errorf("reviewedDoctorCodePrefixes reviews %q*, which doctor.md no longer uses; "+
				"remove it rather than leaving a standing permission", prefix)
		}
	}
	for prefix := range prefixes {
		_, reviewed := reviewedDoctorCodePrefixes[prefix]
		switch {
		case !reviewed:
			t.Errorf("doctor.md covers a family with the prefix %q*, which is not in "+
				"reviewedDoctorCodePrefixes; add it there with the reason the family is one row", prefix)
		case !strings.HasSuffix(prefix, "-") || len(prefix) < minDoctorCodePrefixLen:
			t.Errorf("reviewed prefix %q* is too broad: a family prefix must end at a hyphen "+
				"and be at least %d characters", prefix, minDoctorCodePrefixLen)
		}
	}

	codes := doctor.FindingCodes()
	if len(codes) == 0 {
		t.Fatal("doctor publishes no finding codes; the vocabulary walk looks vacuous")
	}
	for _, code := range codes {
		tok := code.String()
		if exact[tok] {
			continue
		}
		covered := false
		for prefix := range prefixes {
			if _, reviewed := reviewedDoctorCodePrefixes[prefix]; reviewed && strings.HasPrefix(tok, prefix) {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("doctor finding code %q is not documented: doctor.md neither lists it nor "+
				"covers it with a reviewed family prefix", tok)
		}
	}
}

// gcActionHeadings maps the word a gc.md group label must begin with to the action
// that group documents. It is explicit rather than derived from
// CandidateAction.String(): the page reads "Deleted" where the token is "delete", and
// inventing a stemming rule to bridge the two would be more fragile than naming the
// four pairs once.
var gcActionHeadings = map[string]gc.CandidateAction{
	"Deleted":  gc.ActionDelete,
	"Retained": gc.ActionRetain,
	"Blocked":  gc.ActionBlocked,
	"Skipped":  gc.ActionSkipped,
}

// gcReasonTokenRe is one retention-reason spelling. Unlike the doctor codes there are
// no family wildcards here: gc.md owns this vocabulary token by token.
var gcReasonTokenRe = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)

// TestEveryGCRetentionReasonIsDocumented holds the gc retention vocabulary to the
// standard the other two closed machine vocabularies already meet, and one step
// further: gc.md must not only name every token but file each under the action it
// actually justifies.
//
// The action group is worth checking because it is the part a reader acts on. A token
// listed under "Deleted" that the domain treats as a retain reason would send someone
// hunting for data loss that never happened — or, worse, the reverse.
//
// Tokens are read from definition position in the fenced blocks under each group
// label, for the same reason the doctor guard does it: a reason named in a sentence is
// prose, not a definition, and must not be able to satisfy coverage.
func TestEveryGCRetentionReasonIsDocumented(t *testing.T) {
	body, ok := authoredTopicBodies(t)["gc"]
	if !ok {
		t.Fatal("the gc topic is not in the help catalog")
	}

	documented := map[string]gc.CandidateAction{}
	var group gc.CandidateAction
	inFence := false
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, "```"):
			inFence = !inFence
		case inFence:
			if line == "" || line[0] == ' ' || line[0] == '\t' {
				continue // an indented continuation line is a wrapped description
			}
			tok, _, _ := strings.Cut(line, " ")
			if group != 0 && gcReasonTokenRe.MatchString(tok) {
				documented[tok] = group
			}
		case strings.HasPrefix(line, "## "):
			// A new section ends the vocabulary listing, so a later example block
			// cannot be read as if it were still naming reasons.
			group = 0
		default:
			if word, _, _ := strings.Cut(line, " "); gcActionHeadings[word] != 0 {
				group = gcActionHeadings[word]
			}
		}
	}

	reasons := gc.RetentionReasons()
	if len(reasons) == 0 {
		t.Fatal("gc publishes no retention reasons; the vocabulary walk looks vacuous")
	}
	for _, r := range reasons {
		switch documentedAction, listed := documented[r.String()]; {
		case !listed:
			t.Errorf("gc retention reason %q is not documented: gc.md must list it under its "+
				"action group, because that page owns the whole vocabulary", r)
		case documentedAction != r.Action():
			t.Errorf("gc.md documents %q under the %s group, but the domain gives it action %s",
				r, documentedAction, r.Action())
		}
	}
	// Exact spelling, reported as itself: a typo would also show up as a missing
	// reason above, but naming the misspelling is what tells an author where to look.
	for tok := range documented {
		if !gc.RetentionReason(tok).Valid() {
			t.Errorf("gc.md documents %q as a retention reason, but no such reason exists", tok)
		}
	}
}

// minFlagRowsForTableCheck is the size at which a command's flag set counts as a
// "table". Below it, an authored page listing every flag is a short, useful
// enumeration rather than a shadow copy of the generated reference.
const minFlagRowsForTableCheck = 4

// TestExportedPagesCarryEveryAuthoredExitNote proves the published command page is
// never less complete than the terse help for the one field most easily forgotten:
// the exit-status contract. `awa run` returning the child's code and `awa doctor`
// exiting on its own verdict are exactly the facts a script author needs, and they
// live only in authored help metadata — nothing else would notice their absence
// from the export.
func TestExportedPagesCarryEveryAuthoredExitNote(t *testing.T) {
	pages := map[string]string{}
	for _, d := range mustBundle(t).Documents() {
		pages[d.Path().String()] = string(d.Body())
	}

	checked := 0
	for _, c := range newRouter().commands {
		notes := map[string]string{"awa " + c.name: c.help.exitNote}
		for _, sc := range c.subcommands {
			notes["awa "+c.name+" "+sc.name] = sc.help.exitNote
		}
		body, ok := pages[docsCommandsDir+"/"+c.name+".md"]
		if !ok {
			t.Errorf("command %q has no exported page", c.name)
			continue
		}
		for label, note := range notes {
			if note == "" {
				continue
			}
			checked++
			if !strings.Contains(body, note) {
				t.Errorf("%q declares an exit note that its exported page omits: %q", label, note)
			}
		}
	}
	// A zero here means the projection was dropped rather than that no command
	// declares an exit note.
	if checked == 0 {
		t.Error("no exit notes checked; the projection looks lost")
	}
}

// definedFlagRe matches a flag in DEFINITION position: the first thing on its line,
// after optional indentation, an optional list marker, and an optional opening
// backtick. That is the shape of a reference row — "--flag   what it does" — as
// opposed to a flag used inside an example invocation or mentioned mid-sentence.
//
// The distinction is the whole point of the check below. A page explaining how to
// use `awa run show --last --tail 500` is doing its job; a page that has grown a
// column of every flag with its description has become a second copy of a document
// that is generated from the parser.
var definedFlagRe = regexp.MustCompile("(?m)^[ \t]*(?:[-*][ \t]+)?`?(-{1,2}[A-Za-z][A-Za-z0-9-]*)")

// TestNoAuthoredTopicRestatesAWholeFlagTable proves the authored corpus never grows
// a second copy of the generated flag reference. Partial, curated flag lists are
// deliberate and stay allowed — the failure mode this catches is a page whose flag
// enumeration has quietly become exhaustive, which is the point at which it starts
// going stale without anything noticing.
func TestNoAuthoredTopicRestatesAWholeFlagTable(t *testing.T) {
	type cmdFlags struct {
		label string
		toks  []string
	}
	var commands []cmdFlags
	for _, p := range flagProbes() {
		toks := commandFlagTokens(p.help)
		if len(toks) >= minFlagRowsForTableCheck {
			commands = append(commands, cmdFlags{p.label, toks})
		}
	}
	if len(commands) == 0 {
		t.Fatalf("no command has %d+ documented flags; the probe looks vacuous", minFlagRowsForTableCheck)
	}

	for slug, body := range authoredTopicBodies(t) {
		defined := map[string]bool{}
		for _, m := range definedFlagRe.FindAllStringSubmatch(body, -1) {
			defined[m[1]] = true
		}
		for _, c := range commands {
			all := true
			for _, tok := range c.toks {
				if !defined[tok] {
					all = false
					break
				}
			}
			if all {
				t.Errorf("topic %q defines every documented flag of %q as a reference row; the exhaustive "+
					"table is generated from the parser, so an authored copy is a second source of truth", slug, c.label)
			}
		}
	}
}

// TestPrivacyTopicListsEveryInheritedBaselineName keeps the shipped documentation of the
// wrapped-command environment true to the product.
//
// awa help privacy enumerates the built-in baseline by name, because "which variables
// does my command actually see?" is a question a table answers and prose does not. That
// makes it the one place shipped help can silently go stale on a security surface: add
// a name to the baseline and every project inherits it while the page still says
// otherwise. The union across platforms is used so a Windows-only name is documented on
// every platform's copy of the page, which is the same document.
func TestPrivacyTopicListsEveryInheritedBaselineName(t *testing.T) {
	body, ok := authoredTopicBodies(t)["privacy"]
	if !ok {
		t.Fatal("the privacy topic is missing from the catalog")
	}
	text := searchable(body)

	names := domainconfig.BaselineEnvNames("windows") // the union across platforms
	if len(names) == 0 {
		t.Fatal("the baseline names nothing; the assertion looks vacuous")
	}
	for _, name := range names {
		if !strings.Contains(text, searchable(name)) {
			t.Errorf("awa help privacy does not name the inherited baseline variable %q; a variable every project inherits by default must be documented where the environment is explained", name)
		}
	}

	// The reserved marker is documented as what it is, and never as an inherited name.
	if !strings.Contains(text, searchable(domainconfig.WrapperMarkerName+"="+domainconfig.WrapperMarkerValue)) {
		t.Errorf("awa help privacy does not state the injected %s=%s", domainconfig.WrapperMarkerName, domainconfig.WrapperMarkerValue)
	}
}
