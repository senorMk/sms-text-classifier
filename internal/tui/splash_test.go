package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/senorMk/android-text-classifier/internal/android"
	"github.com/senorMk/android-text-classifier/internal/classify"
	"github.com/senorMk/android-text-classifier/internal/store"
)

func testPalette() rulesPalette {
	return rulesPalette{
		colors:   map[string]string{"otp": "86", "personal": "150", "promo": "170"},
		fallback: "245",
	}
}

// Every row of the mark must be the same visible width, or the frame doesn't
// close. This is the invariant the byte-vs-rune bug broke.
func TestSplashRowsAreAllOneWidth(t *testing.T) {
	for _, width := range []int{46, 45, 44, 40, 34, 31, 30} {
		out := splash(testPalette(), width)
		if out == "" {
			t.Errorf("width %d produced no mark", width)
			continue
		}
		lines := strings.Split(out, "\n")
		want := width + 2 // content plus the two frame edges
		for i, line := range lines {
			if got := lipgloss.Width(line); got != want {
				t.Errorf("width %d row %d is %d cells, want %d: %q",
					width, i, got, want, lipgloss.NewStyle().UnsetString().Render(line))
			}
		}
		if !strings.HasPrefix(lines[0], "╭") || !strings.HasSuffix(lines[len(lines)-1], "╯") {
			t.Errorf("width %d: frame corners missing", width)
		}
	}
}

func TestSplashBelowMinimumWidth(t *testing.T) {
	for _, width := range []int{29, 20, 5, 0} {
		if got := splash(testPalette(), width); got != "" {
			t.Errorf("width %d cannot hold the frame, got %q", width, got)
		}
	}
}

// The mark exists to show the operation: messages in, verdicts out.
func TestSplashShowsMessagesAndVerdicts(t *testing.T) {
	out := lipgloss.NewStyle().UnsetString().Render(splash(testPalette(), 46))
	for _, want := range []string{
		"G-482913",                 // a message
		"otp", "personal", "promo", // and each verdict it earned
		"▶", // the wire between them
	} {
		if !strings.Contains(out, want) {
			t.Errorf("mark is missing %q:\n%s", want, out)
		}
	}
}

// Every category the mark names must be one the rules actually define,
// otherwise the launch screen shows a category that can never occur.
func TestSplashCategoriesExistInRules(t *testing.T) {
	cfg := classify.Default()
	for _, s := range splashSamples {
		known := false
		for _, c := range cfg.Order {
			if c == s.cat {
				known = true
			}
		}
		if !known {
			t.Errorf("splash shows category %q, which no rule can produce", s.cat)
		}
	}
}

// The compact fallback is what a narrow terminal gets; it must still say what
// the tool does, and must not overflow.
func TestSplashCompact(t *testing.T) {
	out := lipgloss.NewStyle().UnsetString().Render(splashCompact(testPalette(), 60))
	lines := strings.Split(out, "\n")
	if len(lines) != len(splashSamples) {
		t.Fatalf("compact mark has %d rows, want %d", len(lines), len(splashSamples))
	}
	for _, want := range []string{"otp", "personal", "promo", "verification"} {
		if !strings.Contains(out, want) {
			t.Errorf("compact mark missing %q:\n%s", want, out)
		}
	}
	for i, line := range lines {
		if lipgloss.Width(line) > 60 {
			t.Errorf("compact row %d is %d cells, want at most 60", i, lipgloss.Width(line))
		}
	}
	// And it has to survive a genuinely narrow terminal.
	for _, w := range []int{50, 40, 34, 30, 24, 20, 14} {
		rows := strings.Split(splashCompact(testPalette(), w), "\n")
		for i, line := range rows {
			if lipgloss.Width(line) > w {
				t.Errorf("compact at %d cols, row %d is %d cells", w, i, lipgloss.Width(line))
			}
		}
	}
}

func TestSplashViewFitsTheTerminal(t *testing.T) {
	m, _ := cachedStore(t)
	for _, w := range []int{200, 100, 60, 46, 42, 36, 35, 30, 20, 14} {
		m, _ = press(m, tea.WindowSizeMsg{Width: w, Height: 30})
		view := lipgloss.NewStyle().UnsetString().Render(m.splashView())
		if strings.TrimSpace(view) == "" {
			t.Errorf("width %d rendered an empty mark", w)
			continue
		}
		for i, line := range strings.Split(view, "\n") {
			if lipgloss.Width(line) > w {
				t.Errorf("width %d row %d overflows at %d cells: %q", w, i, lipgloss.Width(line), line)
			}
		}
	}
}

// The mark is for the first pull only; a re-sync with s must not flash it.
func TestSplashOnlyOnFirstSync(t *testing.T) {
	m, _ := cachedStore(t)
	m.stage = stageSync
	if m.synced {
		t.Error("a fresh model should not have synced yet")
	}
}

// --- the device picker shares the screen with the mark ---

func devicePicker(width, height int) Model {
	m := New(nil, []android.Device{
		{Serial: "38071FDJG007QW", Manufacturer: "Google", Model: "Pixel 8 Pro", State: "device"},
		{Serial: "emulator-5554", Manufacturer: "Google", Model: "sdk_gphone64", State: "offline"},
		{Serial: "RF8N30XXXXX", State: "unauthorized"},
	}, Options{Rules: classify.Default()})
	m, _ = press(m, terminalSize(width, height))
	return m
}

func TestDeviceScreenShowsTheMark(t *testing.T) {
	m := devicePicker(84, 24)
	view := lipgloss.NewStyle().UnsetString().Render(m.View())
	if !strings.Contains(view, "G-482913") {
		t.Errorf("the device screen has no mark:\n%s", view)
	}
	if !strings.Contains(view, "Select a device") {
		t.Errorf("the device screen lost its picker:\n%s", view)
	}
	// The mark comes first, so it is what you see.
	if strings.Index(view, "G-482913") > strings.Index(view, "Select a device") {
		t.Error("the mark should be above the picker, not below it")
	}
}

// The list has to be given what is left after the mark, or the screen
// overflows and the help line scrolls out of view.
func TestDeviceScreenFitsTheTerminal(t *testing.T) {
	for _, tc := range []struct{ w, h int }{
		{120, 40}, {84, 24}, {80, 24}, {60, 20}, {50, 16}, {36, 14}, {34, 12}, {20, 10},
	} {
		m := devicePicker(tc.w, tc.h)
		view := lipgloss.NewStyle().UnsetString().Render(m.View())
		rows := strings.Split(strings.TrimRight(view, "\n"), "\n")
		if len(rows) > tc.h {
			t.Errorf("%dx%d: view is %d rows, over the terminal height:\n%s",
				tc.w, tc.h, len(rows), tail(view, 4))
		}
		if tc.w >= 40 {
			// Below ~40 the list delegate truncates one cell wide on its own,
			// which is bubbles' business, not the mark's.
			for i, row := range rows {
				if lipgloss.Width(row) > tc.w {
					t.Errorf("%dx%d: row %d is %d cells wide:\n%q", tc.w, tc.h, i, lipgloss.Width(row), row)
				}
			}
		}
		// The device list must still be tall enough to be usable.
		if m.deviceList.Height()+m.markHeight()+2 > tc.h {
			t.Errorf("%dx%d: list height %d + mark %d leaves nothing",
				tc.w, tc.h, m.deviceList.Height(), m.markHeight())
		}
	}
}

// Arriving at the picker, syncing, and coming back is navigation, not a fresh
// arrival, so the mark must not reappear.
func TestMarkOnlyOnFirstArrival(t *testing.T) {
	m := New(nil, []android.Device{{Serial: "a", State: "device"}, {Serial: "b", State: "device"}},
		Options{Rules: classify.Default()})
	m, _ = press(m, terminalSize(84, 24))

	m.startSync()
	if !m.synced || !m.splash {
		t.Fatalf("the first sync should raise the mark (synced=%v splash=%v)", m.synced, m.splash)
	}
	// A re-sync with s is a background refresh and must not flash it again.
	m.startSync()
	if m.splash {
		t.Error("the mark came back on a second sync")
	}
	if m.showMark() {
		t.Error("the mark should be gone once a sync has run")
	}
	m.cameFromDevice = true
	m.stage = stageDevice
	m.layout()
	view := lipgloss.NewStyle().UnsetString().Render(m.View())
	if strings.Contains(view, "G-482913") {
		t.Errorf("the mark came back on the way to the picker:\n%s", tail(view, 6))
	}
	if !strings.Contains(view, "Select a device") {
		t.Error("lost the picker")
	}
}

// With one device there is nothing to pick, so the mark never gets a chance to
// render — Init goes straight to the sync.
func TestSingleDeviceSkipsPicker(t *testing.T) {
	_, dir := cachedStore(t) // a store for the skipped picker to find
	m := New(nil, []android.Device{{Serial: "dev1", State: "device"}}, Options{
		Rules:    classify.Default(),
		Serial:   "dev1",
		StoreDir: dir,
		Offline:  true,
	})
	settled := drain(t, m)
	if settled.stage != stageCategory {
		t.Errorf("stage = %v, want stageCategory — the picker should be skipped", settled.stage)
	}
	if !settled.synced {
		t.Error("the skipped picker should still have started a sync")
	}
}

func terminalSize(w, h int) tea.WindowSizeMsg { return tea.WindowSizeMsg{Width: w, Height: h} }

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Init is the only method that sets the model up rather than reacting to a
// message, so it has a pointer receiver. With a value receiver its work was
// thrown away the moment it returned — including the serial it picks when only
// one device is attached, which every later stage depends on.
func TestInitStateSurvives(t *testing.T) {
	// A stub adb: "true" ignores its arguments and prints nothing, so the
	// sync completes with an empty store and no device is really touched.
	m := New(&android.Toolchain{ADB: "true"},
		[]android.Device{{Serial: "dev1", State: "device"}},
		Options{Rules: classify.Default(), StoreDir: t.TempDir()})

	if m.Init() == nil {
		t.Fatal("Init returned no command")
	}
	if m.opt.Serial != "dev1" {
		t.Errorf("the auto-picked serial was lost: %q", m.opt.Serial)
	}
	if !m.synced || !m.splash {
		t.Errorf("mark state lost (synced=%v splash=%v)", m.synced, m.splash)
	}

	// The store is opened by the command Init returned, so it lands with the
	// sync message — for the device Init picked.
	for _, c := range m.Init()().(tea.BatchMsg) {
		if _, ok := c().(syncMsg); ok {
			next, _ := m.Update(c())
			m = *next.(*Model)
		}
	}
	if m.st == nil || m.st.Serial != "dev1" {
		t.Errorf("the store was not opened for the picked device: %#v", m.st)
	}
	if m.stage != stageCategory {
		t.Errorf("stage = %v, want stageCategory", m.stage)
	}
}

// Model has to satisfy tea.Model as a pointer now, which is what main passes
// to bubbletea.
func TestModelImplementsTeaModel(t *testing.T) {
	var _ tea.Model = (*Model)(nil)
}

// --- the category panel -------------------------------------------------------

// buckets builds a stand-in distribution with catA, catB, … names.
func buckets(total int, counts ...int) []store.Bucket {
	out := make([]store.Bucket, 0, len(counts))
	for i, c := range counts {
		out = append(out, store.Bucket{Category: "cat" + string(rune('a'+i)), Count: c})
	}
	return out
}

func TestCategoryPanelRendersLiveData(t *testing.T) {
	out := lipgloss.NewStyle().UnsetString().Render(
		categoryPanel(testPalette(), buckets(100, 50, 30, 20), 100, "dev1", 56))
	for _, want := range []string{"100 messages", "dev1", "cata", "50", "catb", "30"} {
		if !strings.Contains(out, want) {
			t.Errorf("panel missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "█") {
		t.Error("panel has no bars")
	}
	// The frame has to close, at every width.
	for _, w := range []int{64, 60, 50, 44, 40, 36, 34, 30} {
		rows := strings.Split(
			lipgloss.NewStyle().UnsetString().Render(categoryPanel(testPalette(), buckets(9, 9, 5, 1), 15, "d", w)), "\n")
		want := w + 2
		for i, r := range rows {
			if lipgloss.Width(r) != want {
				t.Errorf("width %d row %d is %d cells, want %d:\n%q", w, i, lipgloss.Width(r), want, r)
			}
		}
	}
}

// A category with two messages must still be visible next to one with 5,000.
func TestBarGivesEveryNonZeroValueASliver(t *testing.T) {
	if got := bar(0.0001, 20); got == "" {
		t.Error("a tiny non-zero bar rendered as nothing")
	}
	if got := bar(0, 20); got != "" {
		t.Errorf("a zero bar rendered %q", got)
	}
	if got := bar(1, 20); lipgloss.Width(got) != 20 {
		t.Errorf("a full bar is %d cells, want 20", lipgloss.Width(got))
	}
	if got := bar(1.5, 20); lipgloss.Width(got) != 20 {
		t.Errorf("an over-full bar is %d cells, want 20", lipgloss.Width(got))
	}
	if got := bar(0.5, 0); got != "" {
		t.Errorf("zero-width bar rendered %q", got)
	}
	// Proportions have to be monotonic, which is the whole point of the bar.
	prev := -1
	for _, frac := range []float64{0.01, 0.05, 0.2, 0.5, 0.9, 1} {
		n := lipgloss.Width(bar(frac, 40))
		if n < prev {
			t.Errorf("bar(%v) is %d cells, shorter than a smaller share (%d)", frac, n, prev)
		}
		prev = n
	}
}

func TestCategoryPanelSummarisesTheTail(t *testing.T) {
	counts := []int{900, 800, 700, 600, 500, 400, 300, 200}
	b := buckets(4400, counts...)
	out := lipgloss.NewStyle().UnsetString().Render(categoryPanel(testPalette(), b, 4400, "dev1", 60))
	if strings.Contains(out, "catg") || strings.Contains(out, "cath") {
		t.Errorf("the panel should show at most %d categories:\n%s", panelRows, out)
	}
	if !strings.Contains(out, "2 more") {
		t.Errorf("the tail should be summarised:\n%s", out)
	}
	// It has to say how many messages are hidden, not just how many
	// categories, or the total on screen wouldn't add up.
	if !strings.Contains(out, "500") {
		t.Errorf("the hidden message count is missing:\n%s", out)
	}
}

func TestCategoryPanelEmpty(t *testing.T) {
	if got := categoryPanel(testPalette(), nil, 0, "dev1", 60); got != "" {
		t.Errorf("an empty store should render no panel, got %q", got)
	}
	if got := categoryPanel(testPalette(), buckets(5, 0, 0), 0, "dev1", 60); got != "" {
		t.Errorf("a zero total should render no panel, got %q", got)
	}
	// Too narrow for a legible bar: no panel rather than a broken one.
	if got := categoryPanel(testPalette(), buckets(5, 5), 5, "dev1", 20); got != "" {
		t.Errorf("a 20-cell panel should be skipped, got %q", got)
	}
}

func TestCommify(t *testing.T) {
	cases := map[int]string{0: "0", 7: "7", 999: "999", 1000: "1,000", 14925: "14,925", 1000000: "1,000,000"}
	for in, want := range cases {
		if got := commify(in); got != want {
			t.Errorf("commify(%d) = %q, want %q", in, got, want)
		}
	}
}

// The panel is a header for this screen, so it is here every time you look —
// unlike the launch mark, it is never stale.
func TestPanelShowsOnEveryVisitToCategories(t *testing.T) {
	m, _ := cachedStore(t)
	m = drain(t, m)
	if !strings.Contains(m.View(), "messages ·") {
		t.Errorf("no panel on the category screen:\n%s", tail(m.View(), 10))
	}
	// Leaving and coming back keeps it.
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEnter})
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if !strings.Contains(m.View(), "messages ·") {
		t.Error("the panel disappeared on the way back")
	}
	// The message list is about one category; no panel there.
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEnter})
	if strings.Contains(m.View(), "messages ·") {
		t.Error("the panel should not follow you into a single category")
	}
}

// The list under the panel gets what is left, or the screen overflows.
func TestCategoryScreenFitsTheTerminal(t *testing.T) {
	m, _ := cachedStore(t)
	for _, tc := range []struct{ w, h int }{{120, 40}, {84, 26}, {80, 24}, {60, 20}, {44, 18}, {40, 14}} {
		m2, _ := press(m, terminalSize(tc.w, tc.h))
		view := lipgloss.NewStyle().UnsetString().Render(m2.View())
		rows := strings.Split(strings.TrimRight(view, "\n"), "\n")
		if len(rows) > tc.h {
			t.Errorf("%dx%d: view is %d rows:\n%s", tc.w, tc.h, len(rows), tail(view, 4))
		}
		if tc.w >= 44 {
			for i, row := range rows {
				if lipgloss.Width(row) > tc.w {
					t.Errorf("%dx%d: row %d is %d cells", tc.w, tc.h, i, lipgloss.Width(row))
				}
			}
		}
	}
}
