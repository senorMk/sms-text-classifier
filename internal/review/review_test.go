package review

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/senorMk/android-text-classifier/internal/android"
	"github.com/senorMk/android-text-classifier/internal/classify"
	"github.com/senorMk/android-text-classifier/internal/store"
)

// fake is a scripted engine.
type fake struct {
	name    string
	model   string
	verdict func(item Item, categories []string) (Result, error)
	saw     []Item
	cats    []string
	err     error
}

func (f *fake) Name() string  { return or(f.name, "fake") }
func (f *fake) Model() string { return or(f.model, "fake-1") }

func (f *fake) Review(_ context.Context, items []Item, categories []string, onResult func(Result)) (int, error) {
	f.saw = items
	f.cats = categories
	if f.err != nil {
		return 0, f.err
	}
	n := 0
	for _, it := range items {
		if f.verdict == nil {
			continue
		}
		r, err := f.verdict(it, categories)
		if err != nil {
			return n, err
		}
		// An id of 0 is left alone on purpose: engines really do omit ids, and
		// the caller has to cope.
		n++
		onResult(r)
	}
	return n, nil
}

func or(a, b string) string {
	if a == "" {
		return b
	}
	return a
}

// filled builds a store of n rule verdicts with the given confidences.
func filled(t *testing.T, ids []int64, confs ...float64) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir(), "dev1")
	if err != nil {
		t.Fatal(err)
	}
	var msgs []android.Message
	for i, id := range ids {
		c := 0.9
		if i < len(confs) {
			c = confs[i]
		}
		msgs = append(msgs, android.Message{
			ID: id, Address: "+26097100000", Body: fmt.Sprintf("body %d", id),
			Type: android.KindInbox, Date: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC),
			// Classification is filled below via Classify on a temp copy.
		})
		_ = c
	}
	s.Merge(msgs)
	return s
}

// withVerdicts sets explicit confidences on a store's records.
func withVerdicts(s *store.Store, confs map[int64]float64) {
	for i := range s.Records {
		if c, ok := confs[s.Records[i].ID]; ok {
			s.Records[i].Class = store.Class{
				Category: "other", Confidence: c, Source: store.SourceRules, Reason: "test",
			}
		}
	}
}

func cfg() *classify.Config { return classify.Default() }

func TestCandidatesSelectsOnlyTheUndecided(t *testing.T) {
	s := filled(t, []int64{1, 2, 3, 4})
	withVerdicts(s, map[int64]float64{1: 0.95, 2: 0.2, 3: 0.9, 4: 0.5})

	got := Candidates(s, cfg(), Options{Below: 0.55})
	if len(got) != 2 {
		t.Fatalf("candidates = %d, want 2", len(got))
	}
	// Worst first, so a cap spends its budget where it helps.
	if got[0].ID != 2 || got[1].ID != 4 {
		t.Errorf("candidates = %v, want 2 then 4 (worst confidence first)", []int64{got[0].ID, got[1].ID})
	}
}

func TestCandidatesRespectsLimit(t *testing.T) {
	s := filled(t, []int64{1, 2, 3, 4, 5})
	withVerdicts(s, map[int64]float64{1: 0.1, 2: 0.2, 3: 0.3, 4: 0.4, 5: 0.5})
	got := Candidates(s, cfg(), Options{Below: 0.55, Limit: 2})
	if len(got) != 2 || got[0].ID != 1 || got[1].ID != 2 {
		t.Errorf("candidates = %#v, want the two worst", got)
	}
}

func TestRunAppliesVerdicts(t *testing.T) {
	s := filled(t, []int64{1, 2})
	withVerdicts(s, map[int64]float64{1: 0.2, 2: 0.3})

	f := &fake{verdict: func(it Item, _ []string) (Result, error) {
		return Result{ID: it.ID, Category: "otp", Reason: "looks like a code", Confidence: 0.88}, nil
	}}
	n, err := Run(context.Background(), s, cfg(), f, Options{Below: 0.55})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("applied %d, want 2", n)
	}
	for _, r := range s.Records {
		if r.Class.Category != "otp" {
			t.Errorf("id %d = %q, want otp", r.ID, r.Class.Category)
		}
		if r.Class.Source != "fake" {
			t.Errorf("id %d source = %q, want the engine's name", r.ID, r.Class.Source)
		}
		// The engine's own confidence is kept, not flattened to 1.
		if r.Class.Confidence != 0.88 {
			t.Errorf("id %d confidence = %v, want the engine's 0.88", r.ID, r.Class.Confidence)
		}
	}
}

// The guarantee that matters: the rules stay authoritative. A message the
// rules were sure about is never even offered, so no engine can touch it.
func TestEngineCannotOverturnAConfidentRule(t *testing.T) {
	s := filled(t, []int64{1, 2})
	withVerdicts(s, map[int64]float64{1: 0.99, 2: 0.10})

	f := &fake{verdict: func(it Item, _ []string) (Result, error) {
		return Result{ID: it.ID, Category: "spam", Reason: "I feel like spam", Confidence: 1}, nil
	}}
	if _, err := Run(context.Background(), s, cfg(), f, Options{Below: 0.55}); err != nil {
		t.Fatal(err)
	}
	for _, r := range s.Records {
		if r.ID == 1 {
			if r.Class.Category != "other" || r.Class.Source != store.SourceRules {
				t.Errorf("the confident rule verdict was overwritten: %#v", r.Class)
			}
		}
	}
	if len(f.saw) != 1 || f.saw[0].ID != 2 {
		t.Errorf("engine saw %#v, want only the undecided message", f.saw)
	}
}

// An engine answering outside the set it was handed would create a category
// no rule can ever match, so those verdicts are dropped rather than stored.
func TestRunDropsOutOfSetCategories(t *testing.T) {
	s := filled(t, []int64{1, 2})
	withVerdicts(s, map[int64]float64{1: 0.2, 2: 0.3})

	f := &fake{verdict: func(it Item, _ []string) (Result, error) {
		if it.ID == 1 {
			return Result{ID: it.ID, Category: "astrology", Confidence: 0.9}, nil
		}
		return Result{ID: 0, Category: "otp", Confidence: 0.9}, nil // no id
	}}
	n, err := Run(context.Background(), s, cfg(), f, Options{Below: 0.55})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("applied %d, want 0 — both verdicts were unusable", n)
	}
	for _, r := range s.Records {
		if r.Class.Source != store.SourceRules {
			t.Errorf("id %d was overwritten by an unusable verdict", r.ID)
		}
	}
}

func TestRunNilEngine(t *testing.T) {
	s := filled(t, []int64{1})
	withVerdicts(s, map[int64]float64{1: 0.1})
	n, err := Run(context.Background(), s, cfg(), nil, Options{Below: 0.55})
	if n != 0 || err != nil {
		t.Errorf("n = %d err = %v", n, err)
	}
}

func TestRunNothingToDo(t *testing.T) {
	s := filled(t, []int64{1})
	withVerdicts(s, map[int64]float64{1: 0.99})
	f := &fake{verdict: func(it Item, _ []string) (Result, error) { return Result{ID: it.ID}, nil }}
	n, err := Run(context.Background(), s, cfg(), f, Options{Below: 0.55})
	if n != 0 || err != nil {
		t.Errorf("n = %d err = %v", n, err)
	}
	if len(f.saw) != 0 {
		t.Error("the engine should not be called with nothing to judge")
	}
}

func TestRunPassesCategoriesInDisplayOrder(t *testing.T) {
	s := filled(t, []int64{1})
	withVerdicts(s, map[int64]float64{1: 0.2})
	f := &fake{verdict: func(it Item, _ []string) (Result, error) {
		return Result{ID: it.ID, Category: "otp"}, nil
	}}
	if _, err := Run(context.Background(), s, cfg(), f, Options{Below: 0.55}); err != nil {
		t.Fatal(err)
	}
	if len(f.cats) == 0 || f.cats[0] != "otp" {
		t.Errorf("categories = %v, want the configured order", f.cats)
	}
}

func TestRunSurfacesEngineErrors(t *testing.T) {
	s := filled(t, []int64{1})
	withVerdicts(s, map[int64]float64{1: 0.2})
	boom := errors.New("rate limited")
	f := &fake{err: boom}
	if _, err := Run(context.Background(), s, cfg(), f, Options{Below: 0.55}); !errors.Is(err, boom) {
		t.Errorf("err = %v, want the engine's", err)
	}
}

// The state handed to an engine should carry the cheap facts a model would
// otherwise have to rediscover, stated plainly.
func TestFactsAreStatedForTheModel(t *testing.T) {
	facts := factsFor(store.Record{Message: android.Message{
		Address: "FNB", ContainsOTP: true, Type: android.KindInbox,
	}})
	joined := strings.Join(facts, " | ")
	for _, want := range []string{"received", "one-time passcode", "alphanumeric"} {
		if !strings.Contains(joined, want) {
			t.Errorf("facts %q missing %q", joined, want)
		}
	}

	out := factsFor(store.Record{Message: android.Message{
		Address: "+260971234567", Type: android.KindSent,
	}})
	joined = strings.Join(out, " | ")
	if !strings.Contains(joined, "owner") {
		t.Errorf("an outbound message should say so: %q", joined)
	}
	if !strings.Contains(joined, "ordinary phone number") {
		t.Errorf("a long number should be described as a person's: %q", joined)
	}
}

func TestSenderKind(t *testing.T) {
	cases := map[string]string{
		"FNB": "alphanumeric", "Google": "alphanumeric", "+9182": "shortcode",
		"12345": "shortcode", "+260971234567": "longnumber", "a@b.com": "email",
		"": "unknown", "not a sender at all": "alphanumeric",
	}
	for addr, want := range cases {
		if got := senderKind(addr); got != want {
			t.Errorf("senderKind(%q) = %q, want %q", addr, got, want)
		}
	}
}

// A low-confidence model verdict belongs back in the review queue, not
// treated as settled. This is what makes a calibrated confidence worth having.
func TestHesitantModelVerdictGoesBackInTheQueue(t *testing.T) {
	s := filled(t, []int64{1})
	withVerdicts(s, map[int64]float64{1: 0.2})

	f := &fake{verdict: func(it Item, _ []string) (Result, error) {
		return Result{ID: it.ID, Category: "otp", Confidence: 0.3}, nil // still unsure
	}}
	if _, err := Run(context.Background(), s, cfg(), f, Options{Below: 0.55}); err != nil {
		t.Fatal(err)
	}
	queue := s.NeedsReview(0.55)
	if len(queue) != 1 {
		t.Errorf("review queue = %d, want the hesitant verdict back in it", len(queue))
	}
}
