package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/senorMk/android-text-classifier/internal/store"
)

type threadItem struct {
	item
	search string
}

func (i threadItem) FilterValue() string { return i.search }

func (m *Model) showThreads() {
	m.threads = nil
	items := []list.Item{}
	for _, thread := range store.Threads(m.st.Records) {
		matches := m.category == ""
		var search strings.Builder
		for _, r := range thread.Records {
			matches = matches || r.Class.Category == m.category
			fmt.Fprintf(&search, "%s %s %s ", r.Sender(), r.Address, r.Body)
		}
		if !matches {
			continue
		}
		r := thread.Latest()
		items = append(items, threadItem{item: item{
			title: fmt.Sprintf("%s · %d messages · %s", thread.Label(), len(thread.Records), r.Date.Local().Format("02 Jan 15:04")),
			desc:  oneLine(r.Body, 88), selValue: strconv.Itoa(len(m.threads)),
		}, search: search.String()})
		m.threads = append(m.threads, thread)
	}
	title := "Threads"
	if m.category != "" {
		title += " matching " + m.category + " (full conversations)"
	}
	m.msgList = newList(title, items, true)
	m.stage = stageMessage
	m.layout()
}

func (m *Model) selectedThread() int {
	if it, ok := m.msgList.SelectedItem().(threadItem); ok {
		if i, err := strconv.Atoi(it.selValue); err == nil && i >= 0 && i < len(m.threads) {
			return i
		}
	}
	return -1
}

func (m *Model) showThread() tea.Cmd {
	i := m.selectedThread()
	if i < 0 {
		return nil
	}
	m.threadIndex = i
	m.stage = stageThread
	m.layout()
	m.renderThread()
	return nil
}

func (m *Model) renderThread() {
	thread := m.threads[m.threadIndex]
	var b strings.Builder
	fmt.Fprintf(&b, "%s · %d messages (oldest first)\n\n", titleStyle.Render(thread.Label()), len(thread.Records))
	for _, r := range thread.Records {
		fmt.Fprintf(&b, "%s · %s · %s\n%s\n%s\n\n",
			dimStyle.Render(r.Date.Local().Format("02 Jan 2006 15:04")), r.Type.String(), r.Sender(),
			scopeStyle(m.opt.Rules, r.Class.Category).Render(r.Class.Category),
			lipgloss.NewStyle().Width(m.viewerWidth()).Render(r.Body))
	}
	m.viewer.SetContent(b.String())
	m.viewer.GotoTop()
}
