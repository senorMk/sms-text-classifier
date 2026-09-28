// Package review is the second-opinion pass: the deterministic rules decide
// first, and only the residue they were unsure about is offered to a model.
//
// The interface here is deliberately tiny so the engines stay swappable and the
// rules stay authoritative. No engine can overturn a confident rule verdict;
// the most an engine can do is supply a verdict for something the rules left
// open, or supply one the rules already doubted.
package review

import (
	"context"
	"sort"
	"strings"

	"github.com/senorMk/android-text-classifier/internal/classify"
	"github.com/senorMk/android-text-classifier/internal/store"
)

// Item is a message to be judged, trimmed to what a judgment actually needs.
type Item struct {
	ID     int64
	Sender string
	Body   string
	// Facts are the cheap, already-computed signals a rules engine has and
	// a model would otherwise have to rediscover from the text: whether this
	// is an automated sender, and which way the message went.
	Facts []string
}

// Result is one verdict. Confidence is the engine's own certainty where it has
// one; 0 means the engine does not report confidence, and the caller should
// treat the verdict as a decision rather than a measurement.
type Result struct {
	ID         int64
	Category   string
	Reason     string
	Confidence float64
}

// Reviewer is an engine that can judge messages against a fixed category set.
//
// Implementations must not return a category outside the set they were given,
// and should drop ids they did not judge rather than inventing them.
type Reviewer interface {
	// Name is a short label for the footer and for the record's source.
	Name() string
	// Model reports the specific model answering, for the log line.
	Model() string
	// Review judges items against categories, calling onResult for each
	// verdict as it lands. It returns how many verdicts it produced.
	Review(ctx context.Context, items []Item, categories []string, onResult func(Result)) (int, error)
}

// Options configure a pass.
type Options struct {
	// Limit caps how many messages are sent. 0 means no cap, which is a bad
	// idea with a network engine — callers should set one.
	Limit int
	// Below drops the pass entirely, so callers can pass an optional engine
	// straight through without branching.
	Below float64
}

// Candidates returns the records worth sending: the ones the rules were unsure
// about, capped, and nothing else.
func Candidates(st *store.Store, cfg *classify.Config, opts Options) []store.Record {
	var all []store.Record
	for _, r := range st.Records {
		if r.Class.Source == store.SourceRules && r.Class.Confidence < opts.Below {
			all = append(all, r)
		}
	}
	// Worst confidence first: if the cap bites, those are the ones that gain
	// the most from a second opinion.
	sort.SliceStable(all, func(i, j int) bool {
		return all[i].Class.Confidence < all[j].Class.Confidence
	})
	if opts.Limit > 0 && len(all) > opts.Limit {
		all = all[:opts.Limit]
	}
	return all
}

// Run performs the pass against a store and reports how many verdicts landed.
// Verdicts are applied as they arrive so a long run shows progress, and the
// store is saved by the caller.
func Run(ctx context.Context, st *store.Store, cfg *classify.Config, r Reviewer, opts Options) (int, error) {
	if r == nil {
		return 0, nil
	}
	candidates := Candidates(st, cfg, opts)
	if len(candidates) == 0 {
		return 0, nil
	}

	items := make([]Item, 0, len(candidates))
	for _, rec := range candidates {
		items = append(items, Item{
			ID:     rec.ID,
			Sender: rec.Sender(),
			Body:   rec.Body,
			Facts:  factsFor(rec),
		})
	}

	allowed := make(map[string]bool, len(cfg.Categories()))
	for _, c := range cfg.Categories() {
		allowed[c] = true
	}

	applied := 0
	_, err := r.Review(ctx, items, cfg.Categories(), func(res Result) {
		// An engine answering outside the set it was handed has invented
		// something; that would create a category no rule can ever match.
		if res.ID == 0 || !allowed[res.Category] {
			return
		}
		if st.SetVerdict(res.ID, store.Verdict{
			Category:   res.Category,
			Reason:     res.Reason,
			Source:     r.Name(),
			Confidence: res.Confidence,
		}) {
			applied++
		}
	})
	if err != nil {
		return applied, err
	}
	// Trust the count we actually wrote rather than what the engine claimed:
	// some of its verdicts may have been dropped.
	return applied, nil
}

// MaxBodyChars is how much of a message body is sent. Enough for any judgment;
// short enough that one forwarded thread can't dominate the state.
const MaxBodyChars = 1200

// factsFor are the signals a model cannot cheaply derive but a rules engine
// already knows. They are stated as plain facts because these models answer
// the words they are given.
func factsFor(r store.Record) []string {
	var facts []string
	add := func(s string) { facts = append(facts, s) }

	if r.Type == 1 {
		add("The message was received, not sent.")
	} else {
		add("The message was sent by the phone's owner.")
	}
	if r.ContainsOTP {
		add("The phone's own SMS retriever detected a one-time passcode in the body.")
	}
	switch kind := senderKind(r.Address); kind {
	case "shortcode":
		add("The sender is a short code, which machines use rather than people.")
	case "alphanumeric":
		add("The sender is a registered alphanumeric sender id, which an organisation uses rather than a person.")
	case "longnumber":
		add("The sender is an ordinary phone number, which is what a person uses.")
	case "email":
		add("The sender is an email address.")
	}
	return facts
}

func senderKind(address string) string {
	a := strings.TrimSpace(address)
	switch {
	case a == "":
		return "unknown"
	case strings.Contains(a, "@"):
		return "email"
	case isAllDigits(a, 3, 6):
		return "shortcode"
	case isAllDigits(a, 7, 15):
		return "longnumber"
	}
	return "alphanumeric"
}

func isAllDigits(s string, lo, hi int) bool {
	core := strings.TrimPrefix(s, "+")
	if len(core) < lo || len(core) > hi {
		return false
	}
	for _, r := range core {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
