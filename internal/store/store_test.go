package store

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/senorMk/android-text-classifier/internal/android"
	"github.com/senorMk/android-text-classifier/internal/classify"
)

func at(id int64, minsAgo int64, addr, body string) android.Message {
	return android.Message{
		ID:      id,
		Address: addr,
		Body:    body,
		Type:    android.KindInbox,
		Date:    time.UnixMilli(1_790_000_000_000 - minsAgo*60_000).UTC(),
	}
}

func TestOpenEmptyDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "created", "on", "demand")
	s, err := Open(dir, "38071FDJG007QW")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Records) != 0 || s.MaxID() != 0 {
		t.Errorf("fresh store = %d records, max %d", len(s.Records), s.MaxID())
	}
	if got := filepath.Base(s.Path); got != "38071FDJG007QW.jsonl" {
		t.Errorf("path = %s, want a per-serial jsonl", s.Path)
	}
}

func TestMergeDedupesAndKeepsClassifications(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, "dev1")
	if err != nil {
		t.Fatal(err)
	}
	if n := s.Merge([]android.Message{at(1, 5, "A", "first"), at(2, 4, "B", "second")}); n != 2 {
		t.Fatalf("added = %d, want 2", n)
	}
	s.Classify(classify.Default())
	if s.Records[0].Class.Category == "" {
		t.Fatal("Classify left records unclassified")
	}

	// Re-pulling the same ids must not duplicate or reset the verdicts.
	if n := s.Merge([]android.Message{at(1, 5, "A", "first"), at(3, 3, "C", "third")}); n != 1 {
		t.Errorf("added = %d, want 1", n)
	}
	if len(s.Records) != 3 {
		t.Errorf("records = %d, want 3", len(s.Records))
	}
	byID := map[int64]Record{}
	for _, r := range s.Records {
		byID[r.ID] = r
	}
	if byID[1].Class.Category == "" || byID[1].Class.Source != "rules" {
		t.Error("existing classification was lost on merge")
	}
	if byID[3].Class.Category != "" {
		t.Error("the newly merged record should still be unclassified")
	}
	// A zero id is never a real row.
	if n := s.Merge([]android.Message{{Address: "X", Body: "no id"}}); n != 0 {
		t.Errorf("added = %d for an id-less message, want 0", n)
	}
}

func TestRecordsAreSortedNewestFirst(t *testing.T) {
	s, err := Open(t.TempDir(), "dev1")
	if err != nil {
		t.Fatal(err)
	}
	s.Merge([]android.Message{at(1, 60, "A", "old"), at(2, 1, "A", "new"), at(3, 30, "A", "middle")})
	got := []string{s.Records[0].Body, s.Records[1].Body, s.Records[2].Body}
	want := []string{"new", "middle", "old"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestSaveAndReopenRoundTrips(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, "dev1")
	if err != nil {
		t.Fatal(err)
	}
	s.Merge([]android.Message{
		at(10, 1, "Google", "G-123456 is your verification code"),
		at(11, 2, "+260977123456", "line one\nline two, creator=injected"),
	})
	s.Classify(classify.Default())
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	again, err := Open(dir, "dev1")
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Records) != 2 {
		t.Fatalf("reopened with %d records, want 2", len(again.Records))
	}
	if again.Records[0].Class.Category != "otp" {
		t.Errorf("category = %q, want otp", again.Records[0].Class.Category)
	}
	if again.Records[0].Class.Confidence <= 0 {
		t.Error("confidence did not survive the round trip")
	}
	if again.Records[1].Body != "line one\nline two, creator=injected" {
		t.Errorf("multiline body = %q", again.Records[1].Body)
	}
	if again.Meta.Hash == "" || again.Meta.Count != 2 {
		t.Errorf("meta = %#v", again.Meta)
	}
}

// A store can hold private message bodies, so it must not be world-readable.
func TestSaveIsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, "dev1")
	s.Merge([]android.Message{at(1, 1, "A", "secret")})
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{s.Path, s.metaPath} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s mode = %o, want 600", filepath.Base(p), perm)
		}
	}
}

// A half-written last line must not cost the whole store.
func TestLoadSkipsCorruptLines(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, "dev1")
	s.Merge([]android.Message{at(1, 1, "A", "good one"), at(2, 2, "B", "good two")})
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(s.Path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("{not json\n")
	f.Close()

	again, err := Open(dir, "dev1")
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Records) != 2 {
		t.Errorf("records = %d, want 2 (the corrupt line skipped)", len(again.Records))
	}
}

func TestStaleTracksRuleChanges(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, "dev1")
	s.Merge([]android.Message{at(1, 1, "Google", "verification code 1234")})
	s.Classify(classify.Default())
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	same, err := Open(dir, "dev1")
	if err != nil {
		t.Fatal(err)
	}
	if same.Stale(classify.Default()) {
		t.Error("unchanged rules should not be stale")
	}

	edited := filepath.Join(dir, "rules.toml")
	if err := os.WriteFile(edited, []byte("[[rule]]\ncategory = \"x\"\nbody_contains = [\"verification\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := classify.Load(edited)
	if err != nil {
		t.Fatal(err)
	}
	if !same.Stale(changed) {
		t.Error("edited rules should be reported as stale")
	}
}

func TestBucketsFollowConfiguredOrder(t *testing.T) {
	cfg := classify.Default()
	s, _ := Open(t.TempDir(), "dev1")
	s.Merge([]android.Message{
		at(1, 1, "Google", "G-123456 is your verification code"),
		at(2, 2, "Google", "G-654321 is your verification code"),
		at(3, 3, "SHOP", "50% off today only, use code SAVE"),
	})
	s.Classify(cfg)

	buckets := s.Buckets(cfg)
	if len(buckets) != len(cfg.Order) {
		t.Fatalf("buckets = %d, want one per configured category (%d)", len(buckets), len(cfg.Order))
	}
	if buckets[0].Category != "otp" || buckets[0].Count != 2 {
		t.Errorf("first bucket = %#v, want otp with 2", buckets[0])
	}
	// An empty category still gets a row so the picker is stable.
	var travel Bucket
	for _, b := range buckets {
		if b.Category == "travel" {
			travel = b
		}
	}
	if travel.Count != 0 {
		t.Errorf("travel = %#v, want a zero row", travel)
	}
	if promo := s.In("promo"); len(promo) != 1 {
		t.Errorf("In(promo) = %d records, want 1", len(promo))
	}
}

func TestInFiltersByCategory(t *testing.T) {
	s, _ := Open(t.TempDir(), "dev1")
	s.Merge([]android.Message{at(1, 1, "Google", "G-123456 is your verification code")})
	s.Classify(classify.Default())
	if got := s.In("otp"); len(got) != 1 || got[0].ID != 1 {
		t.Errorf("In(otp) = %#v", got)
	}
	if got := s.In("banking"); len(got) != 0 {
		t.Errorf("In(banking) = %d records, want 0", len(got))
	}
}

func TestExport(t *testing.T) {
	s, _ := Open(t.TempDir(), "dev1")
	s.Merge([]android.Message{at(7, 1, "Google", "G-482913 is your Google verification code")})
	s.Classify(classify.Default())

	var js bytes.Buffer
	if err := ExportJSON(&js, s.Records); err != nil {
		t.Fatal(err)
	}
	var back []Record
	if err := json.Unmarshal(js.Bytes(), &back); err != nil {
		t.Fatalf("exported JSON does not round trip: %v", err)
	}
	if len(back) != 1 || back[0].ID != 7 || back[0].Class.Category != "otp" {
		t.Errorf("round trip = %#v", back)
	}

	var csvBuf bytes.Buffer
	if err := ExportCSV(&csvBuf, s.Records); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(csvBuf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("csv lines = %d, want 2: %q", len(lines), csvBuf.String())
	}
	if !strings.HasPrefix(lines[0], "id,date,kind") {
		t.Errorf("csv header = %q", lines[0])
	}
	if !strings.Contains(lines[1], "otp") {
		t.Errorf("csv row = %q", lines[1])
	}
}

func TestMaxID(t *testing.T) {
	s, _ := Open(t.TempDir(), "dev1")
	if s.MaxID() != 0 {
		t.Error("empty store should have MaxID 0")
	}
	s.Merge([]android.Message{at(5, 1, "A", "a"), at(90, 2, "A", "b"), at(40, 3, "A", "c")})
	if s.MaxID() != 90 {
		t.Errorf("MaxID = %d, want 90", s.MaxID())
	}
}

func TestSanitizeSerial(t *testing.T) {
	if got := sanitizeSerial("emulator-5554"); got != "emulator-5554" {
		t.Errorf("got %q", got)
	}
	if got := sanitizeSerial("../../etc/passwd"); strings.Contains(got, "/") || strings.Contains(got, "..") {
		t.Errorf("sanitizeSerial leaked path characters: %q", got)
	}
	if got := sanitizeSerial(""); got != "device" {
		t.Errorf("empty serial = %q, want device", got)
	}
}
