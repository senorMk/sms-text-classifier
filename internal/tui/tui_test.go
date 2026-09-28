package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/senorMk/android-text-classifier/internal/android"
	"github.com/senorMk/android-text-classifier/internal/classify"
	"github.com/senorMk/android-text-classifier/internal/store"
)

func record(id int64, addr, body string) store.Record {
	return store.Record{
		Message: android.Message{
			ID:      id,
			Address: addr,
			Body:    body,
			Type:    android.KindInbox,
			Date:    time.Date(2026, 9, 21, 14, 12, 0, 0, time.UTC),
		},
		Class: store.Class{Category: "personal", Confidence: 0.9, Source: "rules"},
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"exactly-10", 10, "exactly-10"},
		{"one too long", 10, "one too l…"},
		{"", 5, ""},
		{"abc", 1, "…"},
		{"héllo wörld", 6, "héllo…"},
	}
	for _, c := range cases {
		if got := truncate(c.in, c.n); got != c.want {
			t.Errorf("truncate(%q,%d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

func TestOneLine(t *testing.T) {
	// Multi-line and whitespace-heavy bodies have to collapse to one row.
	got := oneLine("line one\n\nline two\t\twith   spaces", 80)
	if got != "line one line two with spaces" {
		t.Errorf("oneLine() = %q", got)
	}
	if long := oneLine(strings.Repeat("word ", 40), 20); len([]rune(long)) != 20 {
		t.Errorf("oneLine() did not truncate: %d runes", len([]rune(long)))
	}
}

func TestPad(t *testing.T) {
	if got := pad("ab", 5); got != "ab   " {
		t.Errorf("pad() = %q", got)
	}
	if got := pad("abcdef", 3); got != "abcdef" {
		t.Errorf("pad() should not truncate, got %q", got)
	}
}

func TestHumanCount(t *testing.T) {
	cases := map[int]string{0: "0", 7: "7", 999: "999", 1000: "1.0k", 14924: "14.9k", 1_500_000: "1.5M"}
	for in, want := range cases {
		if got := humanCount(in); got != want {
			t.Errorf("humanCount(%d) = %q, want %q", in, got, want)
		}
	}
}

// The "?" marker is how a low-confidence verdict is surfaced in the list, so
// it has to track the configured review threshold.
func TestMessageRowMarksUnsure(t *testing.T) {
	m := New(nil, nil, Options{Rules: classify.Default()})
	reviewBelow := m.opt.Rules.ReviewBelow

	sure := record(1, "+260977000111", "on my way")
	if strings.Contains(m.messageRow(sure), "?") {
		t.Errorf("confident row should be unmarked: %q", m.messageRow(sure))
	}

	unsure := record(2, "+260977000111", "hm")
	unsure.Class.Confidence = reviewBelow - 0.01
	if !strings.Contains(m.messageRow(unsure), "?") {
		t.Errorf("unsure row should be marked: %q", m.messageRow(unsure))
	}

	// A verdict that came from the model is never "unsure" — it is decided.
	llm := record(3, "+260977000111", "hm")
	llm.Class.Source = "llm"
	if strings.Contains(m.messageRow(llm), "?") {
		t.Errorf("llm row should be unmarked: %q", m.messageRow(llm))
	}
}

func TestMessageRowIsOneLine(t *testing.T) {
	m := New(nil, nil, Options{Rules: classify.Default()})
	row := m.messageRow(record(1, "+260977000111", "first line\nsecond line"))
	if strings.Contains(row, "\n") {
		t.Errorf("row must not contain newlines: %q", row)
	}
	if !strings.Contains(row, "first line second line") {
		t.Errorf("row lost body text: %q", row)
	}
}

func TestBucketDesc(t *testing.T) {
	if got := bucketDesc(store.Bucket{}); !strings.Contains(got, "empty") {
		t.Errorf("empty bucket = %q", got)
	}
	got := bucketDesc(store.Bucket{Category: "otp", Count: 12, Senders: 3, LowConf: 2})
	for _, want := range []string{"3 sender", "2 unsure"} {
		if !strings.Contains(got, want) {
			t.Errorf("bucketDesc() = %q, missing %q", got, want)
		}
	}
}

// Without a colour for the category the row must still render, just unstyled.
func TestScopeStyleFallsBack(t *testing.T) {
	if s := scopeStyle(classify.Default(), "unknown-category"); s.Render("x") != "x" {
		t.Errorf("uncoloured category should render plainly, got %q", s.Render("x"))
	}
	if s := scopeStyle(classify.Default(), "otp"); !strings.Contains(s.Render("otp"), "otp") {
		t.Errorf("otp lost its text: %q", s.Render("otp"))
	}
}

func TestIsRune(t *testing.T) {
	r := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}}
	if !isRune(r, 's') {
		t.Error("isRune should match a bare rune")
	}
	if isRune(r, 'l') {
		t.Error("isRune matched the wrong rune")
	}
	if isRune(tea.KeyMsg{Type: tea.KeyEsc}, 's') {
		t.Error("isRune matched a non-rune key")
	}
	multi := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s', 'x'}}
	if isRune(multi, 's') {
		t.Error("isRune should not match a multi-rune paste")
	}
}

func TestNewListDisablesQuitKeybindings(t *testing.T) {
	// The app owns esc to walk back up its stages, so the list must not
	// swallow it into a quit.
	l := newList("t", nil, false)
	// DisableQuitKeybindings clears the enabled bit, so pressing esc no longer
	// produces a tea.Quit from inside the list.
	if l.KeyMap.Quit.Enabled() {
		t.Error("list quit keybinding should be disabled")
	}
	if l.KeyMap.ForceQuit.Enabled() {
		t.Error("list force-quit keybinding should be disabled")
	}
}
