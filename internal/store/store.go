// Package store keeps pulled messages on disk so browsing and
// re-classification are instant, and so an incremental pull only has to fetch
// what the device gained since last time.
//
// The on-disk format is JSON Lines — one message per line, human-inspectable,
// and cheap to append to. A sidecar meta file records which rules produced the
// stored classifications, so editing rules.toml re-classifies automatically
// instead of showing stale verdicts.
package store

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/senorMk/android-text-classifier/internal/android"
	"github.com/senorMk/android-text-classifier/internal/classify"
)

// Where a verdict came from.
const (
	SourceRules  = "rules"
	SourceJev    = "jev"
	SourceOpenAI = "openai"
	SourceManual = "manual"
)

// Class is one message's verdict, kept beside the message rather than inside
// it so the device-data type stays free of classification concerns.
type Class struct {
	Category   string    `json:"category,omitempty"`
	Score      float64   `json:"score,omitempty"`
	Confidence float64   `json:"confidence,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	Source     string    `json:"source,omitempty"`
	At         time.Time `json:"at,omitempty"`
}

// FromRules reports whether the rules produced this verdict.
func (c Class) FromRules() bool { return c.Source == SourceRules || c.Source == "" }

// Verdict is a verdict to apply from outside the rule engine.
type Verdict struct {
	Category string
	Reason   string
	Source   string
	// Confidence is the engine's own certainty, or 0 when it reports none.
	Confidence float64
}

// Record is one stored message plus its verdict.
type Record struct {
	android.Message
	Class Class `json:"classification"`
}

// ListSerials returns the serials that have a cache in dir, most recently
// pulled first. It backs --offline, where there is no device to ask which one
// you meant.
func ListSerials(dir string) ([]string, error) {
	if dir == "" {
		var err error
		if dir, err = defaultDir(); err != nil {
			return nil, err
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	type cached struct {
		serial   string
		lastPull time.Time
	}
	var found []cached
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".meta.json") {
			continue
		}
		c := cached{serial: strings.TrimSuffix(name, ".meta.json")}
		if b, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			var meta Meta
			if json.Unmarshal(b, &meta) == nil {
				if meta.Serial != "" {
					c.serial = meta.Serial
				}
				c.lastPull = meta.LastPull
			}
		}
		found = append(found, c)
	}
	sort.SliceStable(found, func(i, j int) bool {
		return found[i].lastPull.After(found[j].lastPull)
	})

	out := make([]string, 0, len(found))
	for _, c := range found {
		out = append(out, c.serial)
	}
	return out, nil
}

// Meta is the sidecar that tells us whether the stored verdicts are still
// current.
type Meta struct {
	Serial   string    `json:"serial"`
	Rules    string    `json:"rules_source"`
	Hash     string    `json:"rules_hash"`
	LastPull time.Time `json:"last_pull"`
	Count    int       `json:"count"`
}

// Store is a device's message cache.
type Store struct {
	Serial   string
	Path     string
	metaPath string
	Meta     Meta
	Records  []Record
}

// Open loads the cache for a serial, returning an empty store if none exists
// yet. An empty dir uses the per-user config directory.
func Open(dir, serial string) (*Store, error) {
	if dir == "" {
		var err error
		dir, err = defaultDir()
		if err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// Serials are used as filenames; keep them to something shell-safe.
	safe := sanitizeSerial(serial)
	s := &Store{
		Serial:   serial,
		Path:     filepath.Join(dir, safe+".jsonl"),
		metaPath: filepath.Join(dir, safe+".meta.json"),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func defaultDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "android-sms-classifier"), nil
}

// sanitizeSerial reduces a serial to a safe filename component. Serials are
// alphanumeric with dashes ("emulator-5554"), but `adb connect` hosts contain
// dots and colons, and nothing here is trusted enough to reach the filesystem
// verbatim.
func sanitizeSerial(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "device"
	}
	return b.String()
}

// load reads the records and meta, tolerating a truncated last line: a store
// killed mid-write should still open.
func (s *Store) load() error {
	if b, err := os.ReadFile(s.metaPath); err == nil {
		_ = json.Unmarshal(b, &s.Meta)
	} else if !os.IsNotExist(err) {
		return err
	}

	f, err := os.Open(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue // skip a corrupt line rather than losing the whole store
		}
		s.Records = append(s.Records, r)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	s.sort()
	return nil
}

func (s *Store) sort() {
	// Newest first; the device returns date DESC but a merged store may not.
	sort.SliceStable(s.Records, func(i, j int) bool {
		return s.Records[i].Date.After(s.Records[j].Date)
	})
}

// Merge adds messages the store doesn't already have and returns how many were
// new. Messages already stored keep their existing classification.
func (s *Store) Merge(msgs []android.Message) int {
	seen := make(map[int64]bool, len(s.Records))
	for _, r := range s.Records {
		seen[r.ID] = true
	}
	added := 0
	for _, m := range msgs {
		if m.ID == 0 || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		s.Records = append(s.Records, Record{Message: m})
		added++
	}
	if added > 0 {
		s.sort()
	}
	return added
}

// MaxID returns the highest message id in the store, or 0 when empty. It is
// the resume point for an incremental pull.
func (s *Store) MaxID() int64 {
	var max int64
	for _, r := range s.Records {
		if r.ID > max {
			max = r.ID
		}
	}
	return max
}

// Messages returns the stored messages in store order.
func (s *Store) Messages() []android.Message {
	out := make([]android.Message, len(s.Records))
	for i, r := range s.Records {
		out[i] = r.Message
	}
	return out
}

// Stale reports whether the stored verdicts were produced by different rules
// than the ones loaded now.
func (s *Store) Stale(cfg *classify.Config) bool {
	return s.Meta.Hash != "" && s.Meta.Hash != cfg.Fingerprint()
}

// NeedsClassify reports whether any record is missing a verdict — a fresh
// store, a partial pull, or a store whose rules changed. Callers must consult
// this *after* merging, since that is what introduces the unclassified rows.
func (s *Store) NeedsClassify(cfg *classify.Config) bool {
	if s.Stale(cfg) {
		return true
	}
	for _, r := range s.Records {
		if r.Class.Source == "" {
			return true
		}
	}
	return false
}

// Classify fills in a verdict for every record, building the corpus once so
// whole-store signals (replied threads, bulk senders) are available.
func (s *Store) Classify(cfg *classify.Config) {
	corpus := classify.NewCorpus(s.Messages())
	now := time.Now().UTC()
	for i := range s.Records {
		res := cfg.Classify(s.Records[i].Message, corpus)
		s.Records[i].Class = Class{
			Category:   res.Category,
			Score:      res.Score,
			Confidence: res.Confidence,
			Reason:     res.Reason,
			Source:     SourceRules,
			At:         now,
		}
	}
	s.Meta.Rules = cfg.Source
	s.Meta.Hash = cfg.Fingerprint()
	s.Meta.Count = len(s.Records)
}

// NeedsReview lists the least confident records, worst first — whichever
// engine produced them. A model verdict that came back unsure belongs in this
// queue just as much as an undecided rule verdict.
func (s *Store) NeedsReview(below float64) []Record {
	var out []Record
	for _, r := range s.Records {
		if r.Class.Confidence < below {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Class.Confidence < out[j].Class.Confidence
	})
	return out
}

// Bucket is one category's slice of the store.
type Bucket struct {
	Category string
	Count    int
	LowConf  int
	Senders  int
}

// Buckets summarizes the store in the configured display order, so a category
// with no messages still appears (as a zero) rather than silently vanishing.
func (s *Store) Buckets(cfg *classify.Config) []Bucket {
	count := map[string]int{}
	low := map[string]int{}
	senders := map[string]map[string]bool{}
	for _, r := range s.Records {
		cat := r.Class.Category
		count[cat]++
		if r.Class.Confidence < cfg.ReviewBelow {
			low[cat]++
		}
		if senders[cat] == nil {
			senders[cat] = map[string]bool{}
		}
		senders[cat][r.Address] = true
	}

	order := cfg.Order
	if len(order) == 0 {
		for cat := range count {
			order = append(order, cat)
		}
		sort.Strings(order)
	}
	out := make([]Bucket, 0, len(order))
	for _, cat := range order {
		out = append(out, Bucket{
			Category: cat,
			Count:    count[cat],
			LowConf:  low[cat],
			Senders:  len(senders[cat]),
		})
	}
	return out
}

// SetVerdict overwrites one record's verdict, keeping its position in the
// store. It is how a review pass layers its verdicts over the rules'.
//
// A verdict that reports no confidence is stored as fully confident, because
// the engine did commit to it; one that reports low confidence is stored
// honestly, which sends it back to the review queue.
func (s *Store) SetVerdict(id int64, v Verdict) bool {
	confidence := v.Confidence
	if confidence <= 0 {
		confidence = 1
	}
	source := v.Source
	if source == "" {
		source = SourceManual
	}
	for i := range s.Records {
		if s.Records[i].ID != id {
			continue
		}
		s.Records[i].Class = Class{
			Category:   v.Category,
			Confidence: confidence,
			Reason:     v.Reason,
			Source:     source,
			At:         time.Now().UTC(),
		}
		return true
	}
	return false
}

// In returns the records of a category, in store order.
func (s *Store) In(category string) []Record {
	var out []Record
	for _, r := range s.Records {
		if r.Class.Category == category {
			out = append(out, r)
		}
	}
	return out
}

// Save writes the store back to disk atomically: a temp file in the same
// directory followed by a rename, so an interrupted write can't truncate the
// cache.
func (s *Store) Save() error {
	tmp, err := os.CreateTemp(filepath.Dir(s.Path), ".sms-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	w := bufio.NewWriter(tmp)
	for _, r := range s.Records {
		b, err := json.Marshal(r)
		if err != nil {
			tmp.Close()
			return err
		}
		w.Write(b)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// The cache holds private message bodies; keep it owner-only.
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), s.Path); err != nil {
		return err
	}

	s.Meta.Count = len(s.Records)
	s.Meta.Serial = s.Serial
	s.Meta.LastPull = time.Now().UTC()
	b, err := json.MarshalIndent(s.Meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.metaPath, b, 0o600)
}

// ExportJSON writes records as a JSON array.
func ExportJSON(w io.Writer, records []Record) error {
	var rows []exportRecord
	if records != nil {
		rows = make([]exportRecord, 0, len(records))
	}
	for _, r := range records {
		rows = append(rows, forExport(r))
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
}

var csvHeader = []string{
	"id", "date", "kind", "read", "address", "person", "category",
	"confidence", "source", "reason", "body", "sender", "receiver",
}

// ExportCSV writes records as CSV.
func ExportCSV(w io.Writer, records []Record) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(csvHeader); err != nil {
		return err
	}
	for _, r := range records {
		participants := forExport(r)
		row := []string{
			fmt.Sprint(r.ID),
			r.Date.Format(time.RFC3339),
			r.Type.String(),
			fmt.Sprint(r.Read),
			r.Address,
			r.Person,
			r.Class.Category,
			fmt.Sprintf("%.2f", r.Class.Confidence),
			r.Class.Source,
			r.Class.Reason,
			r.Body,
			participants.Sender,
			participants.Receiver,
		}
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}
