// Package tui implements the interactive, staged terminal UI:
//
//	pick device -> sync + classify -> pick category -> browse messages -> read
//
// Any stage whose input is already known (single device, --serial) is skipped
// automatically, and the first run pulls the whole store while later runs pull
// only what the device gained.
package tui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/senorMk/android-text-classifier/internal/android"
	"github.com/senorMk/android-text-classifier/internal/classify"
	"github.com/senorMk/android-text-classifier/internal/review"
	"github.com/senorMk/android-text-classifier/internal/store"
)

type stage int

const (
	stageDevice stage = iota
	stageSync
	stageCategory
	stageMessage
	stageViewer
	stageThread
)

var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))
	hintStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	warnStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	categorySty = lipgloss.NewStyle().Bold(true)
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	metaStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
)

// Options configures a session.
type Options struct {
	Serial   string           // pre-picked device (skips the device stage)
	StoreDir string           // cache location; empty uses the user config dir
	Rules    *classify.Config // rule set in force
	Refresh  bool             // re-pull the whole store instead of just the tail
	Offline  bool             // browse the cache without touching adb
	// Reviewer is the optional second-opinion engine; nil disables the pass.
	// review.Run holds the logic, so the UI only decides when to start it.
	Reviewer    review.Reviewer
	ReviewLimit int // cap on messages sent to the model
}

// Model drives the whole interactive session.
type Model struct {
	tc  *android.Toolchain
	opt Options

	stage  stage
	width  int
	height int
	err    error

	// device picker
	deviceList     list.Model
	cameFromDevice bool

	// the store being browsed
	st       *store.Store
	filtered []store.Record
	category string // current scope; "" means all messages
	offset   int    // cursor into filtered, for the reader

	threaded     bool
	threads      []store.Thread
	threadIndex  int
	exportReturn stage

	catList list.Model
	msgList list.Model

	// export prompt, mirroring the pattern of an inline one-line filter
	panel       string // the category histogram, cached per verdicts+width
	exportInput textinput.Model
	exporting   bool
	notice      string

	viewer     viewport.Model
	viewerInit bool

	spinner   spinner.Model
	splash    bool // show the launch mark during the first sync of a session
	synced    bool // so a later re-sync doesn't flash it again
	status    string
	pulled    int
	llmJudged int // how many messages the LLM pass decided, for the footer
	llmCancel context.CancelFunc
	quit      bool
}

// item is a generic list row.
type item struct {
	title    string
	desc     string
	selValue string
	enabled  bool
}

func (i item) Title() string       { return i.title }
func (i item) Description() string { return i.desc }
func (i item) FilterValue() string { return i.title }

// New builds the initial model.
func New(tc *android.Toolchain, devices []android.Device, opt Options) Model {
	sp := spinner.New()
	sp.Spinner = spinner.Dot

	m := Model{tc: tc, opt: opt, spinner: sp}

	ex := textinput.New()
	ex.Prompt = "export › "
	ex.Placeholder = "path (.html, .csv or .json — enter for a default)"

	m.deviceList = newList("Select a device", deviceItems(devices), false)
	m.exportInput = ex
	m.stage = stageDevice
	return m
}

func newList(title string, items []list.Item, showDesc bool) list.Model {
	d := list.NewDefaultDelegate()
	d.ShowDescription = showDesc
	l := list.New(items, d, 0, 0)
	l.Title = title
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(true)
	// The list owns esc/ctrl-c, which this app uses to walk back up its own
	// stages, so those bindings are handled in handleKey instead.
	l.DisableQuitKeybindings()
	l.Styles.Title = titleStyle
	return l
}

func deviceItems(devices []android.Device) []list.Item {
	items := make([]list.Item, 0, len(devices))
	for _, d := range devices {
		items = append(items, item{title: d.Label(), selValue: d.Serial, enabled: d.Ready()})
	}
	return items
}

// Init decides whether to auto-skip the device stage.
//
// A pointer receiver, unlike every other method here: this is the one place
// that sets up the model itself rather than reacting to a message, and with a
// value receiver those assignments would be thrown away the moment it returns.
func (m *Model) Init() tea.Cmd {
	if m.opt.Offline {
		return m.startOffline()
	}
	if m.opt.Serial == "" && len(m.deviceList.Items()) == 1 {
		if it, ok := m.deviceList.Items()[0].(item); ok && it.enabled {
			m.opt.Serial = it.selValue
		}
	}
	if m.opt.Serial != "" {
		return m.startSync()
	}
	return m.spinner.Tick
}

// --- async commands & their result messages ---

// syncMsg reports progress through the sync stages.
type syncMsg struct {
	st           *store.Store // set on the first phase only
	added        int
	total        int
	classified   bool
	needClassify bool
	llmJudged    int
	finishedLLM  bool
	err          error
}

// exportMsg reports where an export landed, or why it didn't.
type exportMsg struct {
	path  string
	count int
	err   error
}

func (m *Model) startSync() tea.Cmd {
	m.stage = stageSync
	// Once per session: a re-sync with s is a background refresh and should
	// not flash the mark over the list.
	m.splash = !m.synced
	m.synced = true
	m.status = "reading message store"
	serial := m.opt.Serial
	tc, opt := m.tc, m.opt
	return tea.Batch(m.spinner.Tick, func() tea.Msg {
		st, err := store.Open(opt.StoreDir, serial)
		if err != nil {
			return syncMsg{err: err}
		}
		// A refresh re-reads ids already held, which Merge would silently
		// discard, so clear first and let it repopulate.
		if opt.Refresh {
			st.Records = nil
		}
		msgs, err := tc.Messages(serial, android.PullOptions{AfterID: st.MaxID()})
		if err != nil {
			return syncMsg{err: err}
		}
		// First run, a partial pull, or rules edited since the last one: the
		// stored verdicts are missing or stale and have to be recomputed.
		added := st.Merge(msgs)
		return syncMsg{st: st, added: added, total: len(msgs), needClassify: st.NeedsClassify(opt.Rules)}
	})
}

// startOffline opens the cache and goes straight to browsing: there is no
// device to pull from, so the only work left is classification.
func (m *Model) startOffline() tea.Cmd {
	m.stage = stageSync
	m.splash = !m.synced
	m.synced = true
	m.status = "reading cached messages"
	serial, rules, dir := m.opt.Serial, m.opt.Rules, m.opt.StoreDir
	return tea.Batch(m.spinner.Tick, func() tea.Msg {
		st, err := store.Open(dir, serial)
		if err != nil {
			return syncMsg{err: err}
		}
		if len(st.Records) == 0 {
			return syncMsg{err: fmt.Errorf("no cached messages for %s", serial)}
		}
		need := st.NeedsClassify(rules)
		if need {
			st.Classify(rules)
		}
		return syncMsg{st: st, classified: need, needClassify: need}
	})
}

// startClassify re-runs the rule engine over the whole store. It is separate
// from the pull only so the status line can say which step is running.
func (m *Model) startClassify() tea.Cmd {
	m.stage = stageSync
	m.status = "classifying"
	st, rules := m.st, m.opt.Rules
	return tea.Batch(m.spinner.Tick, func() tea.Msg {
		st.Classify(rules)
		return syncMsg{classified: true}
	})
}

func (m *Model) startLLM() tea.Cmd {
	m.stage = stageSync
	engine, limit := m.opt.Reviewer, m.opt.ReviewLimit
	st, rules := m.st, m.opt.Rules
	ctx, cancel := context.WithCancel(context.Background())
	m.llmCancel = cancel
	return tea.Batch(m.spinner.Tick, func() tea.Msg {
		if engine == nil {
			return syncMsg{finishedLLM: true}
		}
		n, err := review.Run(ctx, st, rules, engine, review.Options{
			Limit: limit,
			Below: rules.ReviewBelow,
		})
		return syncMsg{llmJudged: n, finishedLLM: true, err: err}
	})
}

// Update is the main event loop.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		// The panel wraps, so a new width means a new panel.
		m.refreshPanel()
		m.layout()
		if m.stage == stageThread {
			m.renderThread()
		}
		return m, nil

	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC {
			m.cancelLLM()
			m.quit = true
			return m, tea.Quit
		}
		return m.handleKey(msg)

	case exportMsg:
		if msg.err != nil {
			// A failed export is worth a notice, not a fatal error: the
			// session is still perfectly usable.
			m.notice = errStyle.Render(msg.err.Error())
		} else {
			m.notice = fmt.Sprintf("exported %d messages → %s", msg.count, msg.path)
		}
		if m.exportReturn == stageThread || (m.exportReturn == stageMessage && m.threaded) {
			m.stage = m.exportReturn
			m.layout()
		} else {
			m.showCategories()
		}
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case syncMsg:
		if msg.err != nil {
			m.cancelLLM()
			m.err = msg.err
			return m, tea.Quit
		}
		if msg.finishedLLM {
			// The model's verdicts are only worth keeping if they land on
			// disk; the rules can always recompute their own.
			if err := m.st.Save(); err != nil {
				m.err = err
				return m, tea.Quit
			}
			m.llmJudged = msg.llmJudged
			m.showCategories()
			return m, nil
		}
		if msg.st != nil {
			m.st = msg.st
			m.pulled = msg.added
			if msg.needClassify {
				return m, m.startClassify()
			}
			// Nothing needed reclassifying, but the new rows still have to be
			// persisted or the next run pulls them again and loses them.
			if err := m.st.Save(); err != nil {
				m.err = err
				return m, tea.Quit
			}
		}
		if msg.classified {
			if err := m.st.Save(); err != nil {
				m.err = err
				return m, tea.Quit
			}
		}
		m.showCategories()
		return m, nil
	}

	var cmd tea.Cmd
	switch m.stage {
	case stageDevice:
		m.deviceList, cmd = m.deviceList.Update(msg)
	case stageCategory:
		m.catList, cmd = m.catList.Update(msg)
	case stageMessage:
		m.msgList, cmd = m.msgList.Update(msg)
	case stageViewer, stageThread:
		m.viewer, cmd = m.viewer.Update(msg)
	}
	return m, cmd
}

func (m *Model) cancelLLM() {
	if m.llmCancel != nil {
		m.llmCancel()
		m.llmCancel = nil
	}
}

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// The export prompt, when open, captures every key.
	if m.exporting {
		return m.handleExportKey(msg)
	}
	m.notice = ""

	switch m.stage {
	case stageDevice:
		if msg.Type == tea.KeyEnter && !m.deviceList.SettingFilter() {
			if it, ok := m.deviceList.SelectedItem().(item); ok {
				if !it.enabled {
					m.err = fmt.Errorf("device %q is not ready", it.selValue)
					return m, nil
				}
				m.opt.Serial = it.selValue
				m.cameFromDevice = true
				return m, m.startSync()
			}
		}
		// esc at the top level quits (when not clearing a filter).
		if msg.Type == tea.KeyEsc && m.deviceList.FilterState() == list.Unfiltered {
			m.quit = true
			return m, tea.Quit
		}
		var cmd tea.Cmd
		m.deviceList, cmd = m.deviceList.Update(msg)
		return m, cmd

	case stageCategory:
		// The action keys are only hijacked while the filter input actually
		// has focus, so a filter you already applied doesn't disable them.
		typing := m.catList.SettingFilter()
		switch {
		case msg.Type == tea.KeyEnter && !m.catList.SettingFilter():
			if it, ok := m.catList.SelectedItem().(item); ok {
				if it.selValue != "" && !it.enabled {
					m.err = fmt.Errorf("no messages in %q — press s to re-sync", it.selValue)
					return m, nil
				}
				m.category = it.selValue
				m.showMessages()
				return m, nil
			}
		case isRune(msg, 's') && !typing:
			// Re-sync picks up whatever arrived since the last pull, which
			// needs a device; offline there is nothing to pull from.
			if m.opt.Offline {
				m.err = errors.New("offline: nothing to sync — drop --offline to reach the device")
				return m, nil
			}
			return m, m.startSync()
		case isRune(msg, 'l') && !typing && m.opt.Reviewer != nil:
			return m, m.startLLM()
		case isRune(msg, 'e') && !typing:
			return m, m.openExport()
		case msg.Type == tea.KeyEsc && m.catList.FilterState() == list.Unfiltered:
			if m.cameFromDevice {
				m.stage = stageDevice
				m.layout()
				return m, nil
			}
			m.quit = true
			return m, tea.Quit
		}
		var cmd tea.Cmd
		m.catList, cmd = m.catList.Update(msg)
		return m, cmd

	case stageMessage:
		typing := m.msgList.SettingFilter()
		switch {
		case msg.Type == tea.KeyEsc && m.msgList.FilterState() == list.Unfiltered:
			m.stage = stageCategory
			m.layout()
			return m, nil
		case msg.Type == tea.KeyEnter && !typing:
			if m.threaded {
				return m, m.showThread()
			}
			return m, m.showViewer()
		case isRune(msg, 't') && !typing:
			m.threaded = !m.threaded
			m.showMessages()
			return m, nil
		case isRune(msg, 'e') && !typing:
			return m, m.openExport()
		}
		var cmd tea.Cmd
		m.msgList, cmd = m.msgList.Update(msg)
		return m, cmd

	case stageThread:
		switch {
		case msg.Type == tea.KeyEsc:
			m.stage = stageMessage
			m.layout()
			return m, nil
		case isRune(msg, 'e'):
			return m, m.openExport()
		}
		var cmd tea.Cmd
		m.viewer, cmd = m.viewer.Update(msg)
		return m, cmd

	case stageViewer:
		// ←/→ (or n/p) move between messages; ↑/↓ stay with the viewport so a
		// message taller than the screen can still be scrolled.
		switch msg.Type {
		case tea.KeyEsc:
			m.stage = stageMessage
			m.layout()
			return m, nil
		case tea.KeyRight:
			return m, m.stepViewer(1)
		case tea.KeyLeft:
			return m, m.stepViewer(-1)
		case tea.KeyRunes:
			if isRune(msg, 'e') {
				return m, m.openExport()
			}
			if isRune(msg, 'n') {
				return m, m.stepViewer(1)
			}
			if isRune(msg, 'p') {
				return m, m.stepViewer(-1)
			}
		}
		var cmd tea.Cmd
		m.viewer, cmd = m.viewer.Update(msg)
		return m, cmd
	}
	return m, nil
}

// --- export ---

// openExport asks where to write, showing the format options in the prompt.
// An empty path means "next to the cache, named after what is on screen".
func (m *Model) openExport() tea.Cmd {
	m.exporting = true
	m.exportInput.Prompt = "export › "
	if m.stage == stageThread || (m.stage == stageMessage && m.threaded) {
		m.exportInput.Prompt = "export full thread › "
	}
	m.exportInput.SetValue("")
	m.exportInput.CursorEnd()
	m.exportInput.Focus()
	m.notice = ""
	return textinput.Blink
}

// handleExportKey drives the export prompt.
func (m *Model) handleExportKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.exporting = false
		m.exportInput.Blur()
		m.layout()
		return m, nil
	case tea.KeyEnter:
		path := strings.TrimSpace(m.exportInput.Value())
		records := m.exportScope()
		if len(records) == 0 {
			m.exporting = false
			m.exportInput.Blur()
			m.notice = "nothing to export"
			m.layout()
			return m, nil
		}
		m.exporting = false
		m.exportInput.Blur()
		m.layout()
		return m, m.startExport(path, records)
	default:
		var cmd tea.Cmd
		m.exportInput, cmd = m.exportInput.Update(msg)
		return m, cmd
	}
}

// exportScope is what "e" exports: the category or "all" currently open, and
// when the list is filtered, only the rows still visible.
func (m *Model) exportScope() []store.Record {
	if m.stage == stageThread {
		return m.threads[m.threadIndex].Records
	}
	if m.stage == stageMessage && m.threaded {
		if i := m.selectedThread(); i >= 0 {
			return m.threads[i].Records
		}
		return nil
	}
	switch m.stage {
	case stageCategory:
		// On the category picker there is no narrower scope than everything.
		return m.st.Records
	case stageViewer:
		// Reading one message: export that one message.
		if m.offset < 0 || m.offset >= len(m.filtered) {
			return nil
		}
		return m.filtered[m.offset : m.offset+1]
	}
	visible := m.msgList.VisibleItems()
	if m.msgList.FilterState() == list.Unfiltered {
		return m.filtered
	}
	ids := make(map[int64]bool, len(visible))
	for _, li := range visible {
		it, ok := li.(item)
		if !ok {
			continue
		}
		if id, err := strconv.ParseInt(it.selValue, 10, 64); err == nil {
			ids[id] = true
		}
	}
	out := make([]store.Record, 0, len(ids))
	for _, r := range m.filtered {
		if ids[r.ID] {
			out = append(out, r)
		}
	}
	return out
}

// startExport writes the export off the main loop, since a 15k-message HTML
// page takes long enough to be visibly janky inline.
func (m *Model) startExport(path string, records []store.Record) tea.Cmd {
	// Captured before the stage moves on: the list is about to be swapped out.
	filtered := m.stage == stageMessage && m.msgList.FilterState() != list.Unfiltered
	scope := m.category
	if m.stage == stageViewer {
		scope = "message-" + strconv.FormatInt(m.currentID(), 10)
	}

	if m.stage == stageThread || (m.stage == stageMessage && m.threaded) {
		scope = "thread-message-" + strconv.FormatInt(records[0].ID, 10)
		filtered = false
	}
	m.exportReturn = m.stage
	m.stage = stageSync
	m.status = fmt.Sprintf("exporting %d messages", len(records))
	st, rules, serial := m.st, m.opt.Rules, m.st.Serial

	return tea.Batch(m.spinner.Tick, func() tea.Msg {
		if path == "" {
			path = defaultExportPath(filepath.Dir(st.Path), serial, scope, filtered)
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			abs = path
		}
		if err := writeExportFile(abs, records, rules, serial); err != nil {
			return exportMsg{err: err}
		}
		return exportMsg{path: abs, count: len(records)}
	})
}

// writeExportFile dispatches on the extension. The format is the only thing
// the path decides, so the TUI and --export can't drift apart.
func writeExportFile(path string, records []store.Record, rules *classify.Config, serial string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("%s already exists", filepath.Base(path))
		}
		return err
	}
	defer f.Close()

	switch strings.ToLower(filepath.Ext(path)) {
	case ".html", ".htm":
		w := bufio.NewWriter(f)
		if err := store.ExportHTML(w, records, store.HTMLOptions{
			Serial:      serial,
			Title:       exportTitle(records, rules),
			Rules:       rules.Source,
			Order:       rules.Order,
			Colors:      rules.Colors,
			ReviewBelow: rules.ReviewBelow,
		}); err != nil {
			return err
		}
		return w.Flush()
	case ".csv":
		return store.ExportCSV(f, records)
	case ".json":
		return store.ExportJSON(f, records)
	}
	return fmt.Errorf("use a .html, .csv or .json extension, got %q", filepath.Ext(path))
}

// defaultExportPath names a file after the current scope, next to the cache,
// without ever clobbering an earlier export.
func defaultExportPath(dir, serial, scope string, filtered bool) string {
	slug := scope
	if slug == "" {
		slug = "all"
	}
	if filtered {
		slug += "-filtered"
	}
	stamp := time.Now().Format("20060102-150405")
	base := filepath.Join(dir, fmt.Sprintf("sms-%s-%s-%s", serial, slug, stamp))
	for i := 1; ; i++ {
		p := base + ".html"
		if i > 1 {
			p = fmt.Sprintf("%s-%d.html", base, i)
		}
		if _, err := os.Stat(p); os.IsNotExist(err) {
			return p
		}
	}
}

// exportTitle names the file after its contents, so a filtered export says
// what is inside when it lands in a downloads folder.
func exportTitle(records []store.Record, rules *classify.Config) string {
	if len(records) == 0 {
		return "SMS messages"
	}
	cats := map[string]bool{}
	for _, r := range records {
		cats[r.Class.Category] = true
	}
	if len(cats) == 1 {
		for c := range cats {
			return "SMS — " + c
		}
	}
	return fmt.Sprintf("SMS messages — %d from %s", len(records), rules.Fingerprint()[:8])
}

// --- stage transitions ---

// showCategories rebuilds the category picker from the current verdicts.
func (m *Model) showCategories() {
	if m.st == nil {
		return
	}
	m.stage = stageCategory

	buckets := m.st.Buckets(m.opt.Rules)
	items := make([]list.Item, 0, len(buckets)+1)
	items = append(items, item{
		title:    "All messages",
		desc:     metaStyle.Render(fmt.Sprintf("%d messages", len(m.st.Records))),
		selValue: "",
	})
	for _, b := range buckets {
		items = append(items, item{
			title:    fmt.Sprintf("%s %s", pad(b.Category, 11), humanCount(b.Count)),
			desc:     bucketDesc(b),
			selValue: b.Category,
			enabled:  b.Count > 0,
		})
	}
	m.catList = newList("Categories", items, true)
	m.refreshPanel()
	m.layout()
}

// bucketDesc summarizes a category's rows, flagging the ones worth a second
// opinion.
func bucketDesc(b store.Bucket) string {
	if b.Count == 0 {
		return dimStyle.Render("empty")
	}
	parts := []string{fmt.Sprintf("%d sender", b.Senders)}
	if b.LowConf > 0 {
		parts = append(parts, warnStyle.Render(fmt.Sprintf("%d unsure", b.LowConf)))
	}
	return metaStyle.Render(strings.Join(parts, " · "))
}

// showMessages opens the message list for the current scope.
func (m *Model) showMessages() {
	if m.threaded {
		m.showThreads()
		return
	}
	if m.category == "" {
		m.filtered = m.st.Records
	} else {
		m.filtered = m.st.In(m.category)
	}
	m.offset = 0

	items := make([]list.Item, 0, len(m.filtered))
	for _, r := range m.filtered {
		items = append(items, item{title: m.messageRow(r), selValue: strconv.FormatInt(r.ID, 10)})
	}
	title := "All messages"
	if m.category != "" {
		title = scopeStyle(m.opt.Rules, m.category).Render(m.category)
	}
	m.msgList = newList(title, items, false)
	m.stage = stageMessage
	m.layout()
}

// showViewer moves to the single-message reader for the highlighted row.
func (m *Model) showViewer() tea.Cmd {
	it, ok := m.msgList.SelectedItem().(item)
	if !ok {
		return nil
	}
	id, err := strconv.ParseInt(it.selValue, 10, 64)
	if err != nil {
		return nil
	}
	for i, r := range m.filtered {
		if r.ID == id {
			m.offset = i
			break
		}
	}
	m.stage = stageViewer
	m.renderViewer()
	m.layout()
	return nil
}

// stepViewer pages between messages.
func (m *Model) stepViewer(delta int) tea.Cmd {
	next := m.offset + delta
	if next < 0 || next >= len(m.filtered) {
		return nil
	}
	m.offset = next
	m.renderViewer()
	return nil
}

// renderViewer fills the viewport with the current message and its metadata.
func (m *Model) renderViewer() {
	if m.offset < 0 || m.offset >= len(m.filtered) {
		return
	}
	r := m.filtered[m.offset]
	var b strings.Builder

	head := []string{
		dimStyle.Render(r.Date.Local().Format("Mon 2 Jan 2006  15:04")),
		dimStyle.Render(r.Type.String()),
	}
	if !r.Read {
		head = append(head, warnStyle.Render("unread"))
	}
	if r.ContainsOTP {
		head = append(head, warnStyle.Render("otp flag"))
	}
	b.WriteString(strings.Join(head, dimStyle.Render("  ·  ")) + "\n")
	if r.Person != "" {
		b.WriteString(titleStyle.Render(r.Person) + "  " + dimStyle.Render(r.Address) + "\n")
	} else {
		b.WriteString(titleStyle.Render(r.Address) + "\n")
	}

	verdict := scopeStyle(m.opt.Rules, r.Class.Category).Render(r.Class.Category) +
		dimStyle.Render(fmt.Sprintf("  %.2f", r.Class.Confidence))
	if r.Class.Source != "" {
		verdict += dimStyle.Render("  " + r.Class.Source)
	}
	b.WriteString(verdict + "\n")
	if r.Class.Reason != "" {
		b.WriteString(dimStyle.Render(r.Class.Reason) + "\n")
	}
	if r.Creator != "" {
		b.WriteString(dimStyle.Render("via "+r.Creator) + "\n")
	}
	b.WriteString("\n")

	// Wrap the body to the viewport so long messages stay readable.
	b.WriteString(lipgloss.NewStyle().Width(m.viewerWidth()).Render(r.Body))
	b.WriteString("\n\n")
	b.WriteString(hintStyle.Render(fmt.Sprintf("message %d of %s", m.offset+1, humanCount(len(m.filtered)))))

	m.initViewer()
	m.viewer.SetContent(b.String())
	m.viewer.GotoTop()
}

// messageRow renders one dense list line: when, who, what. A leading "?" marks
// a verdict the rules were unsure about — exactly the ones worth a look.
func (m *Model) messageRow(r store.Record) string {
	mark := " "
	if r.Class.Source == "rules" && r.Class.Confidence < m.opt.Rules.ReviewBelow {
		mark = "?"
	}
	return fmt.Sprintf("%s %s  %s  %s",
		warnStyle.Render(mark),
		dimStyle.Render(r.Date.Local().Format("02 Jan 15:04")),
		truncate(r.Sender(), 22),
		oneLine(r.Body, 88),
	)
}

// --- layout & rendering ---

func (m *Model) layout() {
	if m.width == 0 || m.height == 0 {
		return
	}
	reserve := 1
	switch m.stage {
	case stageDevice:
		// The device picker shares the screen with the launch mark, so the
		// list only gets what is left over.
		reserve += m.markHeight() + 1
	case stageCategory:
		reserve += m.panelHeight() + 1
	}
	m.deviceList.SetSize(m.width, m.height-reserve)
	// The list widgets are built during a run, and sizing a zero-value list
	// (nil delegate) panics inside updatePagination.
	if m.stage >= stageCategory {
		m.catList.SetSize(m.width, m.height-2-m.panelHeight())
	}
	if m.stage >= stageMessage {
		m.msgList.SetSize(m.width, m.height-2)
	}
	m.initViewer()
}

func (m *Model) initViewer() {
	h := m.height - 2
	if h < 3 {
		h = 3
	}
	if !m.viewerInit {
		m.viewer = viewport.New(m.width, h)
		m.viewerInit = true
		return
	}
	m.viewer.Width = m.width
	m.viewer.Height = h
}

func (m *Model) viewerWidth() int {
	w := m.width - 2
	if w < 20 {
		return 20
	}
	return w
}

func (m *Model) View() string {
	switch m.stage {
	case stageDevice:
		return m.deviceView()
	case stageSync:
		if m.splash {
			// The mark covers the one wait worth covering: the first pull.
			return "\n" + m.splashView() + "\n " + m.spinner.View() + " " + m.status + "\n"
		}
		return fmt.Sprintf("\n %s %s\n", m.spinner.View(), m.status)
	case stageCategory:
		return strings.Join([]string{m.categoryStage()}, "\n")
	case stageMessage:
		return strings.Join([]string{m.msgList.View(), m.footer(m.msgFooter())}, "\n")
	case stageThread:
		return strings.Join([]string{m.viewer.View(), m.footer(hintStyle.Render("↑/↓ scroll · e export full thread · esc back"))}, "\n")
	case stageViewer:
		return strings.Join([]string{m.viewer.View(),
			m.footer(hintStyle.Render("←/→ message · ↑/↓ scroll · e export · esc back"))}, "\n")
	}
	return ""
}

func (m *Model) catFooter() string {
	keys := "enter select · type to filter"
	if !m.opt.Offline {
		keys += " · s re-sync"
	}
	if m.opt.Reviewer != nil {
		keys += " · l " + m.opt.Reviewer.Name() + " pass"
	}
	keys += " · e export"
	back := "esc quit"
	if m.cameFromDevice {
		back = "esc back to devices"
	}
	line := hintStyle.Render(keys + " · " + back)
	notes := ""
	if src := m.opt.Rules.Source; src != "" && src != "<built-in>" {
		notes += "rules: " + src
	}
	if m.llmJudged > 0 {
		if notes != "" {
			notes += "  ·  "
		}
		if m.opt.Reviewer != nil {
			notes += fmt.Sprintf("%s reviewed %d", m.opt.Reviewer.Name(), m.llmJudged)
		}
	}
	if notes != "" {
		line += "\n" + dimStyle.Render(notes)
	}
	return line
}

// footer appends the export prompt or the last notice to a stage's hint line.
func (m *Model) footer(hint string) string {
	if m.exporting {
		return m.exportInput.View()
	}
	if m.notice == "" {
		return hint
	}
	return strings.Join([]string{hint, m.notice}, "\n")
}

func (m *Model) msgFooter() string {
	if m.threaded {
		return hintStyle.Render("enter read thread · / filter · t messages · e export selected thread · esc back")
	}
	scope := m.category
	if scope == "" {
		scope = "all messages"
	}
	return hintStyle.Render(fmt.Sprintf(
		"enter read · type to filter (%d shown) · t threads · e export · esc back · %s",
		len(m.filtered), scope))
}

// Err returns any fatal error captured during the session.
func (m *Model) Err() error { return m.err }

// Quitting reports whether the session ended by user action rather than an
// error, so the caller knows a clean exit from a failure.
func (m *Model) Quitting() bool { return m.quit }

// Pulled reports how many messages the last sync added.
func (m *Model) Pulled() int { return m.pulled }

// --- small helpers ---

func isRune(msg tea.KeyMsg, r rune) bool {
	return msg.Type == tea.KeyRunes && len(msg.Runes) == 1 && msg.Runes[0] == r
}

func scopeStyle(cfg *classify.Config, category string) lipgloss.Style {
	st := categorySty
	if c, ok := cfg.Colors[category]; ok && c != "" {
		st = st.Foreground(lipgloss.Color(c))
	}
	return st
}

// truncate shortens s to n runes, marking that it was cut.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}

// oneLine flattens a body to a single line for list display.
func oneLine(s string, n int) string {
	return truncate(strings.Join(strings.Fields(s), " "), n)
}

func pad(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}

// humanCount abbreviates thousands, so a four-digit message count doesn't
// shove the category names off a narrow terminal.
func humanCount(n int) string {
	if n < 1000 {
		return strconv.Itoa(n)
	}
	if n < 1_000_000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
}
