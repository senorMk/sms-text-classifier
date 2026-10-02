package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/senorMk/android-text-classifier/internal/android"
	"github.com/senorMk/android-text-classifier/internal/classify"
	"github.com/senorMk/android-text-classifier/internal/store"
)

// cachedStore writes a small classified store and returns the model under test.
func cachedStore(t *testing.T) (Model, string) {
	t.Helper()
	dir := t.TempDir()

	s, err := store.Open(dir, "dev1")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 21, 14, 12, 0, 0, time.UTC)
	s.Merge([]android.Message{
		{ID: 1, Address: "Google", Body: "G-538204 is your Google verification code", Type: android.KindInbox, Date: now},
		{ID: 2, Address: "543", Body: "Thank you, your payment of K5.00 to LUPIYA FINANCIAL SERVICES", Type: android.KindInbox, Date: now.Add(-time.Hour)},
		{ID: 3, Address: "+260977000111", Body: "on my way", Type: android.KindInbox, Date: now.Add(-2 * time.Hour), ThreadID: 9},
		{ID: 4, Address: "+260977000111", Body: "sure", Type: android.KindSent, Date: now.Add(-3 * time.Hour), ThreadID: 9},
	})
	s.Classify(classify.Default())
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	m := New(nil, nil, Options{
		Serial:   "dev1",
		StoreDir: dir,
		Rules:    classify.Default(),
		Offline:  true,
	})
	m, _ = press(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	return m, dir
}

// drain runs the commands from Init until nothing is left, the way the runtime
// would, and returns the settled model.
func drain(t *testing.T, m Model) Model {
	t.Helper()
	cmd := m.Init()
	for i := 0; cmd != nil && i < 20; i++ {
		msg := cmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
			// Only the data-bearing command matters here; spinner ticks
			// would just loop forever.
			for _, c := range batch {
				if r := c(); r != nil {
					next, _ := m.Update(r)
					m = *next.(*Model)
				}
			}
			break
		}
		if msg == nil {
			break
		}
		next, nextCmd := m.Update(msg)
		m = *next.(*Model)
		cmd = nextCmd
	}
	return m
}

func TestOfflineReachesCategoryStage(t *testing.T) {
	m, _ := cachedStore(t)
	m = drain(t, m)

	if m.stage != stageCategory {
		t.Fatalf("stage = %v, want stageCategory", m.stage)
	}
	if m.st == nil || len(m.st.Records) != 4 {
		t.Fatalf("store not loaded: %#v", m.st)
	}
	items := m.catList.Items()
	// "All messages" plus one row per configured category.
	if len(items) != len(m.opt.Rules.Order)+1 {
		t.Fatalf("rows = %d, want %d", len(items), len(m.opt.Rules.Order)+1)
	}
	if it, ok := items[0].(item); !ok || it.title != "All messages" {
		t.Errorf("first row = %#v, want All messages", items[0])
	}
	// Enter on "All messages" opens the whole store.
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.stage != stageMessage {
		t.Fatalf("stage = %v, want stageMessage", m.stage)
	}
	if len(m.msgList.Items()) != 4 {
		t.Errorf("message rows = %d, want 4", len(m.msgList.Items()))
	}
}

func TestOfflineScopesToACategory(t *testing.T) {
	m, _ := cachedStore(t)
	m = drain(t, m)

	// Move to the otp row and open it.
	for i, li := range m.catList.Items() {
		it, ok := li.(item)
		if !ok || it.selValue != "otp" {
			continue
		}
		m.catList.Select(i)
		break
	}
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.category != "otp" {
		t.Fatalf("category = %q, want otp", m.category)
	}
	if got := len(m.msgList.Items()); got != 1 {
		t.Errorf("otp rows = %d, want 1", got)
	}
}

func TestEmptyCategoryIsRefused(t *testing.T) {
	m, _ := cachedStore(t)
	m = drain(t, m)

	for i, li := range m.catList.Items() {
		it, ok := li.(item)
		if !ok || it.selValue != "travel" {
			continue
		}
		m.catList.Select(i)
		break
	}
	before := m.stage
	m, cmd := press(m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Error("selecting an empty category should not start a command")
	}
	if m.stage != before {
		t.Errorf("stage moved to %v, want to stay put", m.stage)
	}
	if m.Err() == nil {
		t.Error("expected an explanation for the refused selection")
	}
}

func TestViewerPaging(t *testing.T) {
	m, _ := cachedStore(t)
	m = drain(t, m)
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEnter}) // All messages
	if m.stage != stageMessage {
		t.Fatal("did not reach the message list")
	}

	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEnter}) // open the reader
	if m.stage != stageViewer {
		t.Fatalf("stage = %v, want stageViewer", m.stage)
	}
	first := m.offset
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyRight})
	if m.offset != first+1 {
		t.Errorf("offset = %d, want %d", m.offset, first+1)
	}
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if m.offset != first+2 {
		t.Errorf("n should page forward, offset = %d", m.offset)
	}
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyLeft})
	if m.offset != first+1 {
		t.Errorf("offset = %d, want %d", m.offset, first+1)
	}
	// Paging past the ends is a no-op, not an index panic.
	for range 20 {
		m, _ = press(m, tea.KeyMsg{Type: tea.KeyRight})
	}
	if m.offset != len(m.filtered)-1 {
		t.Errorf("offset = %d, want the last message %d", m.offset, len(m.filtered)-1)
	}
	// esc walks back to the list, then to the categories.
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.stage != stageMessage {
		t.Errorf("stage = %v, want stageMessage", m.stage)
	}
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.stage != stageCategory {
		t.Errorf("stage = %v, want stageCategory", m.stage)
	}
}

func TestViewerRendersMetadataAndBody(t *testing.T) {
	m, _ := cachedStore(t)
	m = drain(t, m)
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEnter})
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEnter})

	view := m.viewer.View()
	for _, want := range []string{"Google", "inbox", "otp", "G-538204"} {
		if !strings.Contains(view, want) {
			t.Errorf("viewer is missing %q:\n%s", want, view)
		}
	}
}

func TestSyncKeyIsRefusedOffline(t *testing.T) {
	m, _ := cachedStore(t)
	m = drain(t, m)
	before := m.stage

	m, cmd := press(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	if cmd != nil {
		t.Error("s should not start a sync with no device")
	}
	if m.stage != before {
		t.Errorf("stage = %v, want to stay at %v", m.stage, before)
	}
	if m.Err() == nil {
		t.Error("expected an error explaining why")
	}
}

// The footer must not advertise a key that cannot work in this mode.
func TestFooterHidesSyncWhenOffline(t *testing.T) {
	m, _ := cachedStore(t)
	m = drain(t, m)
	if strings.Contains(m.catFooter(), "re-sync") {
		t.Errorf("offline footer should not offer re-sync: %q", m.catFooter())
	}
}

func TestFatalErrorFromSyncQuits(t *testing.T) {
	m, _ := New(nil, nil, Options{Rules: classify.Default(), Offline: true}), ""
	m, cmd := press(m, syncMsg{err: errBoom{}})
	if cmd == nil {
		t.Fatal("expected a quit command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("expected tea.QuitMsg")
	}
	if m.Err() == nil {
		t.Error("the error should be captured for the caller")
	}
}

func TestBucketsNeverLoseACategory(t *testing.T) {
	// Every configured category needs a row, including the empty ones, or the
	// picker would change shape between runs.
	m, _ := cachedStore(t)
	m = drain(t, m)
	seen := map[string]bool{}
	for _, li := range m.catList.Items() {
		if it, ok := li.(item); ok {
			seen[it.selValue] = true
		}
	}
	for _, c := range m.opt.Rules.Order {
		if !seen[c] {
			t.Errorf("category %q has no row", c)
		}
	}
}

// press sends one key to the model and unwraps the tea.Model it returns.
func press(m Model, msg tea.Msg) (Model, tea.Cmd) {
	next, cmd := m.Update(msg)
	return *next.(*Model), cmd
}

type errBoom struct{}

func (errBoom) Error() string { return "boom" }

// --- export ---

// pressExport types a path into the export prompt and submits it, returning the
// model after the resulting command has run.
func pressExport(t *testing.T, m Model, path string) (Model, string) {
	t.Helper()
	m, cmd := press(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	if !m.exporting {
		t.Fatal("e did not open the export prompt")
	}
	if strings.Contains(m.footer(m.catFooter()), m.exportInput.Prompt) == false {
		t.Error("the export prompt is not on screen")
	}
	for _, r := range path {
		m, _ = press(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	m, cmd = press(m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("submitting the prompt produced no command")
	}
	// The command returns a batch (spinner + the write); run the second one.
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatal("expected a batch command")
	}
	var out exportMsg
	for _, c := range batch {
		if msg, ok := c().(exportMsg); ok {
			out = msg
		}
	}
	m, _ = press(m, out)
	return m, out.path
}

func TestExportFromMessageStage(t *testing.T) {
	m, dir := cachedStore(t)
	m = drain(t, m)

	// Narrow to a category, then export just it.
	for i, li := range m.catList.Items() {
		if it, ok := li.(item); ok && it.selValue == "otp" {
			m.catList.Select(i)
			break
		}
	}
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.category != "otp" {
		t.Fatalf("category = %q", m.category)
	}

	m, path := pressExport(t, m, filepath.Join(dir, "out.html"))
	if m.stage != stageCategory {
		t.Errorf("stage = %v, want back at stageCategory", m.stage)
	}
	if !strings.Contains(m.notice, "exported 1 message") {
		t.Errorf("notice = %q", m.notice)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	page := string(data)
	if !strings.Contains(page, "G-538204") {
		t.Error("the exported page is missing the otp message")
	}
	if strings.Contains(page, "balance of K5.00") {
		t.Error("the export leaked messages from another category")
	}
	// Private mail, so the file must not be world-readable.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
}

// Pressing enter with no path picks a default next to the cache.
func TestExportDefaultPath(t *testing.T) {
	m, dir := cachedStore(t)
	m = drain(t, m)
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEnter}) // All messages

	m, path := pressExport(t, m, "")
	if filepath.Dir(path) != dir {
		t.Errorf("default path %s is not next to the cache in %s", path, dir)
	}
	if ext := filepath.Ext(path); ext != ".html" {
		t.Errorf("default ext = %q, want .html", ext)
	}
	if !strings.Contains(filepath.Base(path), "all") {
		t.Errorf("default name %q should say what it holds", filepath.Base(path))
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("default export was not written: %v", err)
	}
}

// With the list filtered, "e" means "what I can see", not "everything".
func TestExportRespectsActiveFilter(t *testing.T) {
	m, dir := cachedStore(t)
	m = drain(t, m)
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEnter}) // All messages

	if got := m.exportScope(); len(got) != 4 {
		t.Fatalf("unfiltered scope = %d records, want 4", len(got))
	}

	// Narrow the list the same way typing into its filter would.
	m.msgList.SetFilterText("Google")
	if got := m.exportScope(); len(got) != 1 {
		t.Errorf("filtered scope = %d records, want 1", len(got))
	}

	m, path := pressExport(t, m, filepath.Join(dir, "filtered.html"))
	if !strings.Contains(filepath.Base(path), "filtered") {
		t.Logf("note: explicit path given, name not auto-derived: %s", path)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "G-538204") {
		t.Error("filtered export lost the matching message")
	}
	if strings.Contains(string(data), "on my way") {
		t.Error("filtered export included a message that was filtered out")
	}
}

func TestExportFormats(t *testing.T) {
	for _, tc := range []struct{ ext, want string }{
		{".csv", "id,date,kind"},
		{".json", "\"classification\""},
		{".html", "<!doctype html>"},
		{".HTML", "<!doctype html>"},
		{".htm", "<!doctype html>"},
	} {
		t.Run(tc.ext, func(t *testing.T) {
			m, dir := cachedStore(t)
			m = drain(t, m)
			m, _ = press(m, tea.KeyMsg{Type: tea.KeyEnter})
			m, path := pressExport(t, m, filepath.Join(dir, "out"+tc.ext))
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), tc.want) {
				t.Errorf("%s export is missing %q", tc.ext, tc.want)
			}
		})
	}
}

func TestExportRejectsBadExtension(t *testing.T) {
	m, dir := cachedStore(t)
	m = drain(t, m)
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEnter})

	m, _ = press(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	for _, r := range filepath.Join(dir, "out.txt") {
		m, _ = press(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	m, cmd := press(m, tea.KeyMsg{Type: tea.KeyEnter})
	batch := cmd().(tea.BatchMsg)
	var out exportMsg
	for _, c := range batch {
		if msg, ok := c().(exportMsg); ok {
			out = msg
		}
	}
	m, _ = press(m, out)
	if out.err == nil {
		t.Fatal("a .txt export should have failed")
	}
	// A failed export is a notice, not a dead session.
	if m.stage != stageCategory || m.Err() != nil {
		t.Errorf("stage = %v err = %v, want a usable session", m.stage, m.Err())
	}
	if !strings.Contains(stripANSI(m.notice), ".html") {
		t.Errorf("notice = %q, want it to name the valid extensions", m.notice)
	}
}

// An export must never silently overwrite an earlier one.
func TestExportRefusesToClobber(t *testing.T) {
	m, dir := cachedStore(t)
	m = drain(t, m)
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEnter})

	target := filepath.Join(dir, "taken.html")
	if err := os.WriteFile(target, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	pressExport(t, m, target)

	data, err := os.ReadFile(target)
	if err != nil || string(data) != "precious" {
		t.Errorf("existing file was overwritten: %q %v", data, err)
	}
}

func TestDefaultExportPathNeverRepeats(t *testing.T) {
	dir := t.TempDir()
	var paths []string
	for range 3 {
		p := defaultExportPath(dir, "dev1", "otp", false)
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	uniq := map[string]bool{}
	for _, p := range paths {
		if uniq[p] {
			t.Fatalf("defaultExportPath repeated %s", p)
		}
		uniq[p] = true
		if !strings.Contains(filepath.Base(p), "dev1") || !strings.Contains(filepath.Base(p), "otp") {
			t.Errorf("default name %q should carry the serial and category", filepath.Base(p))
		}
	}
}

func TestExportEscCancels(t *testing.T) {
	m, _ := cachedStore(t)
	m = drain(t, m)
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	if !m.exporting {
		t.Fatal("prompt did not open")
	}
	stage := m.stage
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.exporting {
		t.Error("esc did not close the prompt")
	}
	if m.stage != stage {
		t.Errorf("esc also moved the stage to %v", m.stage)
	}
}

func TestExportIsOffTheMainLoop(t *testing.T) {
	m, _ := cachedStore(t)
	m = drain(t, m)
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEnter})
	before := m.stage
	m, cmd := press(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.stage != stageSync {
		t.Errorf("stage = %v, want stageSync while writing", m.stage)
	}
	if cmd == nil {
		t.Error("no command returned")
	}
	_ = before
	// The write must not have happened inline: nothing is on disk yet.
	if m.notice != "" {
		t.Error("notice set before the command ran")
	}
}

func stripANSI(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '\x1b' {
			continue
		}
		out = append(out, r)
	}
	return strings.TrimSpace(string(out))
}

// Reading a message and exporting it should give you that message and nothing
// else.
func TestExportSingleMessageFromReader(t *testing.T) {
	m, dir := cachedStore(t)
	m = drain(t, m)
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEnter}) // All messages
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEnter}) // open the reader
	if m.stage != stageViewer {
		t.Fatalf("stage = %v, want stageViewer", m.stage)
	}

	scope := m.exportScope()
	if len(scope) != 1 {
		t.Fatalf("exportScope() in the reader returned %d records, want 1", len(scope))
	}
	if scope[0].ID != m.filtered[m.offset].ID {
		t.Error("the scope is not the message on screen")
	}

	// The reader's own hint line has to advertise the key.
	view := lipgloss.NewStyle().UnsetString().Render(m.View())
	if !strings.Contains(view, "e export") {
		t.Errorf("the reader does not mention export:\n%s", view)
	}

	m, path := pressExport(t, m, filepath.Join(dir, "one.html"))
	if !strings.Contains(m.notice, "exported 1 message") {
		t.Errorf("notice = %q", m.notice)
	}
	page, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(page), "<tr data-id="); n != 1 {
		t.Errorf("export has %d rows, want 1:\n%s", n, excerptRows(string(page)))
	}
}

// Paging with n/p then exporting must follow the message now on screen.
func TestExportSingleMessageFollowsTheCursor(t *testing.T) {
	m, dir := cachedStore(t)
	m = drain(t, m)
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEnter})
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEnter})

	first := m.currentID()
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if m.currentID() == first {
		t.Fatal("n did not advance")
	}
	want := m.currentID()

	m, path := pressExport(t, m, filepath.Join(dir, "second.html"))
	page, _ := os.ReadFile(path)
	if !strings.Contains(string(page), fmt.Sprintf(`data-id="%d"`, want)) {
		t.Errorf("export did not follow the cursor to id %d", want)
	}
	if strings.Contains(string(page), fmt.Sprintf(`data-id="%d"`, first)) {
		t.Errorf("export still contains the message the cursor left, id %d", first)
	}
}

// The default filename for a single message should identify which one.
func TestExportSingleMessageDefaultName(t *testing.T) {
	m, _ := cachedStore(t)
	m = drain(t, m)
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEnter})
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEnter})
	id := m.currentID()

	m, _ = press(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	_, cmd := press(m, tea.KeyMsg{Type: tea.KeyEnter})
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatal("expected a batch command")
	}
	var out exportMsg
	for _, c := range batch {
		if msg, ok := c().(exportMsg); ok {
			out = msg
		}
	}
	if out.err != nil {
		t.Fatalf("export failed: %v", out.err)
	}
	if !strings.Contains(filepath.Base(out.path), fmt.Sprint(id)) {
		t.Errorf("default name %q should carry the message id %d", filepath.Base(out.path), id)
	}
}

func excerptRows(page string) string {
	i := strings.Index(page, "<tbody>")
	if i < 0 {
		return ""
	}
	j := strings.Index(page[i:], "</tbody>")
	if j < 0 {
		return ""
	}
	return page[i : i+j]
}
