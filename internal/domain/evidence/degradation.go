package evidence

// BoundedCount records the outcome of a scan that may have stopped before reading
// everything: how many entries were shown, the total when it is known, and the cap
// that was applied. It makes a bounded sample an honest sample — a consumer can tell
// "16 of 4000, incomplete" from "16 of 16, complete" and never mistake a capped scan
// for an exhaustive one.
//
// It is constructor-built with unexported fields so an impossible shape (a total
// smaller than what was shown, or a "complete" scan whose shown count is below its
// total) cannot be represented.
type BoundedCount struct {
	shown    int
	total    int
	hasTotal bool
	limit    int
}

// NewBoundedCount builds a bounded count from an exhaustive total. shown is how many
// entries were read; total is the full count that exists; limit is the cap that was
// applied (0 means no cap was hit). shown is clamped to [0, total] so the value can
// never claim to have read more than exists.
func NewBoundedCount(shown, total, limit int) BoundedCount {
	if shown < 0 {
		shown = 0
	}
	if total < 0 {
		total = 0
	}
	if shown > total {
		shown = total
	}
	if limit < 0 {
		limit = 0
	}
	return BoundedCount{shown: shown, total: total, hasTotal: true, limit: limit}
}

// NewBoundedCountUnknownTotal builds a bounded count whose total is unknown because
// the scan stopped early or the enumeration itself failed. It never invents a total.
func NewBoundedCountUnknownTotal(shown, limit int) BoundedCount {
	if shown < 0 {
		shown = 0
	}
	if limit < 0 {
		limit = 0
	}
	return BoundedCount{shown: shown, hasTotal: false, limit: limit}
}

// Shown reports how many entries the scan read.
func (b BoundedCount) Shown() int { return b.shown }

// Total reports the full count and whether it is known. When the second return is
// false the total is genuinely unknown and callers must not treat Shown as the total.
func (b BoundedCount) Total() (int, bool) { return b.total, b.hasTotal }

// Limit reports the cap that was applied, or 0 when no cap was involved.
func (b BoundedCount) Limit() int { return b.limit }

// Complete reports whether the scan read every entry that exists. It is true only
// when the total is known and everything was shown; an unknown total is never
// complete.
func (b BoundedCount) Complete() bool { return b.hasTotal && b.shown >= b.total }

// Degradation is one structured durable-read fact: which component it concerns, the
// stable token classifying it, an optional bounded sample, and a human detail. It is
// the wire-facing replacement for prose warning strings — JSON carries the token and
// component so an agent acts without parsing text, while the detail feeds the stderr
// diagnostic a human reads.
//
// It is constructor-built with unexported fields: a Degradation cannot exist without
// a valid component and token, and a read-bounded degradation cannot exist without a
// bounded sample, so an under-specified fact is unrepresentable rather than a
// caller-discipline hazard.
type Degradation struct {
	component Component
	token     DiagnosticToken
	detail    string
	sample    *BoundedCount
}

// NewDegradation builds a degradation for a component and token. It returns ok false
// when the component or token is invalid, or when the token is read-bounded (which
// requires a sample — use NewBoundedDegradation). Callers surface a build failure as
// an internal error rather than emitting an unclassified fact.
func NewDegradation(component Component, token DiagnosticToken, detail string) (Degradation, bool) {
	if !component.Valid() || !token.Valid() || token == TokenReadBounded {
		return Degradation{}, false
	}
	return Degradation{component: component, token: token, detail: detail}, true
}

// NewBoundedDegradation builds a read-bounded degradation, which must carry the bound
// that was applied. token is fixed to read-bounded; the sample records what was
// counted vs skipped. It returns ok false only for an invalid component.
func NewBoundedDegradation(component Component, sample BoundedCount, detail string) (Degradation, bool) {
	if !component.Valid() {
		return Degradation{}, false
	}
	s := sample
	return Degradation{component: component, token: TokenReadBounded, detail: detail, sample: &s}, true
}

// Component returns the affected component.
func (d Degradation) Component() Component { return d.component }

// Token returns the stable diagnostic token.
func (d Degradation) Token() DiagnosticToken { return d.token }

// Detail returns the human detail (may be empty).
func (d Degradation) Detail() string { return d.detail }

// Sample returns the bounded sample and whether one is present. Only a read-bounded
// degradation carries one.
func (d Degradation) Sample() (BoundedCount, bool) {
	if d.sample == nil {
		return BoundedCount{}, false
	}
	return *d.sample, true
}
