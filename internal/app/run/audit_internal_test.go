package run

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"awarer/internal/app/help"
	"awarer/internal/domain/runcache"
)

// auditRecord is one stored candidate as the fake store holds it: either valid
// metadata of a given size, or a read failure.
type auditRecord struct {
	entry runcache.RunEntry
	size  int64
	err   error
}

// auditFakeStore serves a fixed newest-first history to the audit. It embeds a nil
// runcache.Store, so any call outside selection, the history probe, and the bounded
// metadata read —
// lookup, payload, manifest, or the unbounded Get — panics: the audit's only store
// access is the one under test.
type auditFakeStore struct {
	runcache.Store
	newestFirst  []runcache.RunID
	records      map[runcache.RunID]auditRecord
	listErr      error
	historyErr   error
	noHistory    bool
	historyAsked bool
	listLimit    int
	read         []runcache.RunID
	onRead       func(n int)
}

func (f *auditFakeStore) ListRefsNewest(_ context.Context, limit int) ([]runcache.RunID, error) {
	f.listLimit = limit
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.newestFirst[:min(limit, len(f.newestFirst))], nil
}

func (f *auditFakeStore) HistoryPresent() (bool, error) {
	f.historyAsked = true
	return !f.noHistory, f.historyErr
}

func (f *auditFakeStore) GetWithin(id runcache.RunID, maxBytes int64) (runcache.RunEntry, int64, error) {
	f.read = append(f.read, id)
	if f.onRead != nil {
		f.onRead(len(f.read))
	}
	rec := f.records[id]
	if rec.err != nil {
		return runcache.RunEntry{}, 0, rec.err
	}
	if rec.size > maxBytes {
		return runcache.RunEntry{}, 0, fmt.Errorf("%w: test", runcache.ErrReadBudgetExceeded)
	}
	return rec.entry, rec.size, nil
}

// auditHistory builds a fake history from newest-first records, assigning ids whose
// order matches the slice order.
func auditHistory(t *testing.T, recs ...auditRecord) *auditFakeStore {
	t.Helper()
	f := &auditFakeStore{records: map[runcache.RunID]auditRecord{}}
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).UnixNano()
	for i, rec := range recs {
		id := mkRunID(t, base-int64(i)*int64(time.Second), string(rune('a'+i%26)))
		rec.entry.ID = id
		if rec.size == 0 {
			rec.size = 100
		}
		f.newestFirst = append(f.newestFirst, id)
		f.records[id] = rec
	}
	return f
}

// mustReuse unwraps a reuse-state constructor in a fixture; a rejected fixture pair
// is a broken test, not a condition under test.
func mustReuse(s runcache.ReuseState, err error) runcache.ReuseState {
	if err != nil {
		panic(err)
	}
	return s
}

func valid(reuse runcache.ReuseState, exit int) auditRecord {
	return auditRecord{entry: runcache.RunEntry{Reuse: reuse, Exit: runcache.ExitStatus{Kind: runcache.ExitNormal, Code: exit}}}
}

func failed(err error) auditRecord { return auditRecord{err: err} }

// classCounts flattens a result into "group kind/reason" → count, the form the
// expectations below are written in by hand.
func classCounts(res AuditResult) map[string]int {
	out := map[string]int{}
	for _, c := range res.Classes {
		out[fmt.Sprintf("%s %s/%s", c.Group, c.Kind, c.Reason)] = c.Count
	}
	return out
}

func availCounts(res AuditResult) map[AuditAvailability]int {
	out := map[AuditAvailability]int{}
	for _, a := range res.Availability {
		out[a.State] = a.Count
	}
	return out
}

// TestAuditCountsEachRecordOnceByItsStoredPrimaryReason covers every canonical
// stored pair, with expectations written independently of the classifier. A
// --record run over piped input is stored as record-only/record-only — its
// primary reason masks stdin-not-keyed — and must count as deliberate recording
// only. Historical child failures are classified by their stored reason, never
// turned into an audit failure, and a failed run stored reusable stays reusable.
func TestAuditCountsEachRecordOnceByItsStoredPrimaryReason(t *testing.T) {
	recordOnly := func(r runcache.MismatchReason) runcache.ReuseState {
		return mustReuse(runcache.RecordOnly(r))
	}
	nonReusable := func(r runcache.MismatchReason) runcache.ReuseState {
		return mustReuse(runcache.NonReusableRun(r))
	}
	store := auditHistory(t,
		valid(runcache.ReusableCacheEntry(), 0),
		valid(runcache.ReusableCacheEntry(), 3),         // a cached failure (cache_failed_runs)
		valid(recordOnly(runcache.ReasonRecordOnly), 0), // --record with piped stdin
		valid(recordOnly(runcache.ReasonRecordOnly), 0),
		valid(recordOnly(runcache.ReasonNoCache), 0),
		valid(recordOnly(runcache.ReasonStdinNotKeyed), 0),
		valid(recordOnly(runcache.ReasonSkippedInputs), 0),
		valid(recordOnly(runcache.ReasonFastTrustMode), 0),
		valid(nonReusable(runcache.ReasonMutatedState), 0),
		valid(nonReusable(runcache.ReasonEffectStateDiffers), 0),
		valid(nonReusable(runcache.ReasonEffectStateUnavailable), 0),
		valid(runcache.UnknownPostState(), 0),
		valid(nonReusable(runcache.ReasonFailedNotCached), 1),
		valid(nonReusable(runcache.ReasonFailedNotCached), 2),
	)
	res, err := auditRecorded(context.Background(), store, AuditRequest{})
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	want := map[string]int{
		"reusable-when-stored reusable/":                                2,
		"deliberate-recording record-only/record-only":                  2,
		"deliberate-recording record-only/no-cache":                     1,
		"policy-blocker record-only/stdin-not-keyed":                    1,
		"policy-blocker record-only/skipped-inputs":                     1,
		"policy-blocker record-only/fast-trust-mode":                    1,
		"changed-state non-reusable/mutated-state":                      1,
		"changed-state non-reusable/effect-state-differs":               1,
		"observation-unavailable non-reusable/effect-state-unavailable": 1,
		"observation-unavailable unknown-post-state/post-scan-failed":   1,
		"failure-cache-policy non-reusable/failed-not-cached":           2,
	}
	got := classCounts(res)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("class counts:\n got %v\nwant %v", got, want)
	}
	if res.Selected != 14 || res.Classified != 14 || res.Unavailable != 0 || res.Older {
		t.Errorf("selected/classified/unavailable/older = %d/%d/%d/%v, want 14/14/0/false", res.Selected, res.Classified, res.Unavailable, res.Older)
	}
	sum := 0
	for _, c := range res.Classes {
		sum += c.Count
	}
	if sum != res.Classified {
		t.Errorf("class counts sum to %d, classified is %d", sum, res.Classified)
	}
	if res.BytesRead != 14*100 {
		t.Errorf("bytes read = %d, want %d", res.BytesRead, 14*100)
	}
}

// TestAuditWindowIsFixedBeforeClassification proves no refill: invalid, vanished,
// incompatible, and unreadable candidates keep their places in the window, and an
// older valid record past the window is never read in their stead. The one extra
// selected id only reports that older history exists.
func TestAuditWindowIsFixedBeforeClassification(t *testing.T) {
	store := auditHistory(t,
		valid(runcache.ReusableCacheEntry(), 0),
		failed(fmt.Errorf("%w: bad", runcache.ErrCorruptStore)),
		failed(fmt.Errorf("%w: gone", runcache.ErrNotFound)),
		failed(fmt.Errorf("%w: v9", runcache.ErrIncompatibleEntry)),
		failed(errors.New("permission denied")),
		valid(runcache.ReusableCacheEntry(), 0), // older valid candidate
	)
	res, err := auditRecorded(context.Background(), store, AuditRequest{Limit: 5})
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if store.listLimit != 6 {
		t.Errorf("selection asked for %d ids, want limit+1 = 6", store.listLimit)
	}
	if len(store.read) != 5 || store.read[4] == store.newestFirst[5] {
		t.Errorf("read %d candidates; the older record must not be read", len(store.read))
	}
	if res.Requested != 5 || res.Selected != 5 || !res.Older || res.Classified != 1 || res.Unavailable != 4 {
		t.Errorf("requested/selected/older/classified/unavailable = %d/%d/%v/%d/%d, want 5/5/true/1/4",
			res.Requested, res.Selected, res.Older, res.Classified, res.Unavailable)
	}
	wantAvail := map[AuditAvailability]int{AuditCorrupt: 1, AuditVanished: 1, AuditIncompatible: 1, AuditUnreadable: 1}
	if got := availCounts(res); fmt.Sprint(got) != fmt.Sprint(wantAvail) {
		t.Errorf("availability = %v, want %v", got, wantAvail)
	}
	if res.Availability[0].Examples.IDs[0] != store.newestFirst[1] {
		t.Errorf("corrupt example = %v, want the selected candidate %v", res.Availability[0].Examples.IDs, store.newestFirst[1])
	}
}

// TestAuditDefaultWindowAndEmptyHistory pins the default window and that empty and
// missing history are distinct known states with a zero window, while a failure to
// select the window or to establish the history is an error, never a healthy empty
// sample. A selected run is present history without a separate probe.
func TestAuditDefaultWindowAndEmptyHistory(t *testing.T) {
	store := auditHistory(t)
	res, err := auditRecorded(context.Background(), store, AuditRequest{})
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if store.listLimit != DefaultAuditLimit+1 || res.Requested != DefaultAuditLimit {
		t.Errorf("default window: listed %d, requested %d", store.listLimit, res.Requested)
	}
	if res.History != AuditHistoryEmpty || res.Selected != 0 || res.Older || res.Classified != 0 || res.Unavailable != 0 || len(res.Classes) != 0 {
		t.Errorf("empty history result = %+v", res)
	}

	store.noHistory = true
	if res, err = auditRecorded(context.Background(), store, AuditRequest{}); err != nil || res.History != AuditHistoryMissing || res.Selected != 0 {
		t.Errorf("missing history result = %+v, err %v", res, err)
	}

	store.historyErr = fmt.Errorf("%w: entries is not a directory", runcache.ErrCorruptStore)
	if _, err := auditRecorded(context.Background(), store, AuditRequest{}); !errors.Is(err, runcache.ErrCorruptStore) {
		t.Errorf("history failure err = %v, want it returned", err)
	}
	store.historyErr = nil

	populated := auditHistory(t, valid(runcache.ReusableCacheEntry(), 0))
	if res, err = auditRecorded(context.Background(), populated, AuditRequest{}); err != nil || res.History != AuditHistoryPresent || populated.historyAsked {
		t.Errorf("populated history = %v (asked %v), err %v", res.History, populated.historyAsked, err)
	}

	store.listErr = fmt.Errorf("%w: misplaced entry", runcache.ErrCorruptStore)
	if _, err := auditRecorded(context.Background(), store, AuditRequest{}); !errors.Is(err, runcache.ErrCorruptStore) {
		t.Errorf("selection failure err = %v, want it returned", err)
	}
	if _, err := auditRecorded(context.Background(), auditHistory(t), AuditRequest{Limit: MaxAuditLimit + 1}); err == nil {
		t.Error("a window over the published limit must be refused")
	}
}

// TestAuditReadBudgets exercises the per-record and aggregate byte bounds with
// otherwise valid metadata: an oversized record is unmeasured, not corrupt; once
// the aggregate budget cannot cover the next record, the rest of the window is
// left unread and disclosed, and bytes read never exceed the aggregate budget.
func TestAuditReadBudgets(t *testing.T) {
	sized := func(n int64) auditRecord {
		r := valid(runcache.ReusableCacheEntry(), 0)
		r.size = n
		return r
	}
	store := auditHistory(t, sized(50), sized(150), sized(80), sized(80), sized(80), sized(50))
	res, err := auditRecorded(context.Background(), store, AuditRequest{recordBudget: 100, aggregateBudget: 250})
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if res.Classified != 3 || res.Unavailable != 3 || res.BytesRead != 210 {
		t.Errorf("classified/unavailable/bytes = %d/%d/%d, want 3/3/210", res.Classified, res.Unavailable, res.BytesRead)
	}
	want := map[AuditAvailability]int{AuditOverRecordBudget: 1, AuditUnreadBudget: 2}
	if got := availCounts(res); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("availability = %v, want %v", got, want)
	}
	if len(store.read) != 5 {
		t.Errorf("read attempts = %d, want 5 (the tail past an exhausted budget is not read)", len(store.read))
	}
	if res.RecordBudget != 100 || res.AggregateBudget != 250 {
		t.Errorf("budgets = %d/%d", res.RecordBudget, res.AggregateBudget)
	}
}

// TestAuditExamplesAreBoundedNewestFirst proves examples are capped independently
// of the count they sample.
func TestAuditExamplesAreBoundedNewestFirst(t *testing.T) {
	var recs []auditRecord
	for range 7 {
		recs = append(recs, valid(runcache.ReusableCacheEntry(), 0))
	}
	store := auditHistory(t, recs...)
	res, err := auditRecorded(context.Background(), store, AuditRequest{})
	if err != nil {
		t.Fatal(err)
	}
	ex := res.Classes[0].Examples
	if res.Classes[0].Count != 7 || ex.Total != 7 || len(ex.IDs) != AuditExampleLimit {
		t.Fatalf("count/total/shown = %d/%d/%d", res.Classes[0].Count, ex.Total, len(ex.IDs))
	}
	for i, id := range ex.IDs {
		if id != store.newestFirst[i] {
			t.Errorf("example %d = %v, want %v", i, id, store.newestFirst[i])
		}
	}
}

// TestAuditCancellationFailsTheAudit pins that cancellation mid-window is an error,
// never a short successful report.
func TestAuditCancellationFailsTheAudit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := auditHistory(t,
		valid(runcache.ReusableCacheEntry(), 0),
		valid(runcache.ReusableCacheEntry(), 0),
		valid(runcache.ReusableCacheEntry(), 0),
	)
	store.onRead = func(n int) {
		if n == 1 {
			cancel()
		}
	}
	if _, err := auditRecorded(ctx, store, AuditRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(store.read) != 1 {
		t.Errorf("read %d records after cancellation, want 1", len(store.read))
	}
}

// TestAuditServiceUsesOnlyStoredMetadata runs the audit through the service with
// every other port absent: a scan, effect observation, environment read, or clock
// would dereference a nil dependency, and a payload, manifest, lookup, or unbounded
// read would hit the fake's nil embedded store.
func TestAuditServiceUsesOnlyStoredMetadata(t *testing.T) {
	store := auditHistory(t,
		valid(runcache.ReusableCacheEntry(), 0),
		valid(mustReuse(runcache.NonReusableRun(runcache.ReasonMutatedState)), 0),
	)
	svc := &Service{deps: Deps{Store: store}}
	res, err := svc.Audit(context.Background(), AuditRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Classified != 2 {
		t.Errorf("classified = %d, want 2", res.Classified)
	}
}

// TestAuditClassifiesEveryAdmittedStoredPair guards the closed vocabulary: every
// kind/reason pair the stored-metadata reader admits has a classification.
func TestAuditClassifiesEveryAdmittedStoredPair(t *testing.T) {
	kinds := []runcache.ReuseKind{runcache.ReuseReusable, runcache.ReuseNonReusable, runcache.ReuseRecordOnly, runcache.ReuseUnknownPostState}
	reasons := append([]runcache.MismatchReason{""}, runcache.MismatchReasons()...)
	admitted := 0
	for _, k := range kinds {
		for _, r := range reasons {
			if _, err := runcache.NewReuseState(k, r); err != nil {
				continue
			}
			admitted++
			if auditClassIndex(k, r) < 0 {
				t.Errorf("admitted stored pair %s/%s has no audit classification", k, r)
			}
		}
	}
	if admitted != len(auditClasses) {
		t.Errorf("%d admitted pairs, %d classes", admitted, len(auditClasses))
	}
}

// TestEveryAuditTokenIsDocumented keeps the shipped JSON help in step with the
// audit's closed vocabularies: every group, availability state, and history state a
// run.audit document can carry is named in the json topic.
func TestEveryAuditTokenIsDocumented(t *testing.T) {
	var body string
	for _, topic := range help.Topics() {
		if topic.Slug.String() == "json" {
			body = topic.Body
		}
	}
	if body == "" {
		t.Fatal("the json topic is not in the help catalog")
	}
	tokens := []string{string(AuditHistoryPresent), string(AuditHistoryEmpty), string(AuditHistoryMissing)}
	for _, c := range auditClasses {
		tokens = append(tokens, string(c.group))
	}
	for _, a := range auditAvailabilities {
		tokens = append(tokens, string(a))
	}
	for _, tok := range tokens {
		if !strings.Contains(body, "`"+tok+"`") {
			t.Errorf("run.audit token %q is not documented in json.md", tok)
		}
	}
}
