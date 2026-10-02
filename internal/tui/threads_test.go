package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/senorMk/android-text-classifier/internal/store"
)

func openTestThreads(t *testing.T) (Model, string) {
	m, dir := cachedStore(t)
	m = drain(t, m)
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEnter})
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	return m, dir
}

func TestThreadNavigationAndExport(t *testing.T) {
	for _, fromReader := range []bool{false, true} {
		m, dir := openTestThreads(t)
		if !m.threaded || len(m.threads) != 3 {
			t.Fatal("thread toggle failed")
		}
		m.msgList.SetFilterText("on my way")
		if len(m.msgList.VisibleItems()) != 1 {
			t.Fatal("thread body search failed")
		}
		if fromReader {
			m, _ = press(m, tea.KeyMsg{Type: tea.KeyEnter})
			if m.stage != stageThread {
				t.Fatal("thread did not open")
			}
			view := m.viewer.View()
			for _, want := range []string{"sure", "on my way", "sent", "inbox"} {
				if !strings.Contains(view, want) {
					t.Fatalf("missing %q in thread", want)
				}
			}
			if strings.Index(view, "sure") > strings.Index(view, "on my way") {
				t.Fatal("thread is not oldest first")
			}
		}
		before := m.stage
		m, path := pressExport(t, m, filepath.Join(dir, "thread.json"))
		if m.stage != before {
			t.Fatal("export lost browsing position")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var records []store.Record
		if err := json.Unmarshal(data, &records); err != nil {
			t.Fatal(err)
		}
		if len(records) != 2 || records[0].ID != 4 || records[1].ID != 3 {
			t.Fatalf("wrong thread export: %s", data)
		}
		if fromReader {
			m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
			if m.stage != stageMessage || m.selectedThread() < 0 {
				t.Fatal("back lost selected thread")
			}
		}
	}
}

func TestThreadCategoryIncludesWholeConversation(t *testing.T) {
	m, _ := openTestThreads(t)
	m.category = "otp"
	for i := range m.st.Records {
		if m.st.Records[i].ID == 3 {
			m.st.Records[i].Class.Category = "otp"
		}
	}
	m.showThreads()
	m.msgList.SetFilterText("on my way")
	if got := m.exportScope(); len(got) != 2 {
		t.Fatalf("lost reply from other category: %+v", got)
	}
	m.msgList.SetFilterText("no such conversation")
	if len(m.exportScope()) != 0 {
		t.Fatal("empty thread search exported records")
	}
	m.msgList.ResetFilter()
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	if m.threaded || m.stage != stageMessage || len(m.filtered) != 2 {
		t.Fatal("message toggle lost category")
	}
	m.msgList.SetFilterText("no such message")
	if len(m.exportScope()) != 0 {
		t.Fatal("empty message search exported records")
	}
}

func TestThreadExportFormatsAndDefaultName(t *testing.T) {
	for _, ext := range []string{"html", "csv", "json", ""} {
		m, dir := openTestThreads(t)
		m.msgList.SetFilterText("on my way")
		path := ""
		if ext != "" {
			path = filepath.Join(dir, "thread."+ext)
		}
		_, path = pressExport(t, m, path)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "on my way") || !strings.Contains(string(data), "sure") || strings.Contains(string(data), "G-538204") {
			t.Fatalf("wrong contents in %s", path)
		}
		if ext == "" && !strings.Contains(filepath.Base(path), "thread-message-4") {
			t.Fatalf("wrong default name: %s", path)
		}
	}
}
