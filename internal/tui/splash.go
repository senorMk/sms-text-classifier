package tui

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/senorMk/android-text-classifier/internal/store"
)

// The launch mark. It is not a wordmark: the thing this tool actually does is
// take rows of messages and put a label on each one, so that is what is drawn —
// a phone screen with three messages, each wired to the category it landed in.
//
// Every colour comes from the rules file, so the mark shows the live palette
// and editing rules.toml changes it.

// splashSample is one wireframe message and the category it should render as.
type splashSample struct {
	body string
	cat  string
}

var splashSamples = []splashSample{
	{"G-482913 is your verification code", "otp"},
	{"are you coming tonight?", "personal"},
	{"50% off everything today", "promo"},
}

// splashPad insets the text inside the frame; splashArrow heads the wire from a
// message to its verdict.
var (
	splashPad   = "  "
	splashArrow = "▶ "
)

// splashFrameMin is the narrowest frame that still fits the longest verdict
// ("personal") beside a readable message; splashMinWidth is the terminal width
// that affords it, and below that only the verdicts are shown.
const (
	splashFrameMin = 30
	splashMinWidth = splashFrameMin + 6
	// splashMinHeight is how much room the mark needs beside the device
	// picker. Below this the picker keeps every row it can get, because it is
	// the part you have to use.
	splashMinHeight = 18
)

// splashView renders the launch mark fitted to the terminal, or the compact
// list when there isn't room for the frame.
func (m *Model) splashView() string {
	pal := rulesPalette{colors: m.opt.Rules.Colors, fallback: "245"}
	if m.width < splashMinWidth {
		return splashCompact(pal, m.width)
	}
	// Leave room for the indent and a right margin, and cap it so the mark
	// stays a mark rather than stretching across a wide terminal.
	w := m.width - 6
	if w > 46 {
		w = 46
	}
	return "  " + splash(pal, w)
}

// currentID is the message the reader is on, for naming a single export.
func (m *Model) currentID() int64 {
	if m.offset < 0 || m.offset >= len(m.filtered) {
		return 0
	}
	return m.filtered[m.offset].ID
}

// splash renders the launch mark, fitted to width.
func splash(cfg categoryPalette, width int) string {
	edge := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	const tagRoom = 1 // never let the wire touch the text

	if width < splashFrameMin {
		return ""
	}
	// Measured in cells, not bytes: the arrow head is a multi-byte rune and
	// len() would silently mis-size every row.
	padW, arrowW := lipgloss.Width(splashPad), lipgloss.Width(splashArrow)

	rows := []string{edge.Render("╭" + strings.Repeat("─", width) + "╮")}
	for _, s := range splashSamples {
		tagW := lipgloss.Width(s.cat)
		tag := cfg.style(s.cat).Render(s.cat)
		// Reserve room for the wire and the tag before wrapping, so the
		// last line of a message always has somewhere left to put them.
		avail := width - 2*padW - arrowW - tagW - tagRoom
		wrapped := wrapBody(s.body, avail)
		for i, line := range wrapped {
			row := padRight(splashPad+line, width)
			if i == len(wrapped)-1 {
				gap := width - lipgloss.Width(splashPad+line) - arrowW - tagW - padW
				if gap < 1 {
					gap = 1
				}
				wire := edge.Render(strings.Repeat("─", gap) + splashArrow)
				row = padRight(splashPad+line+wire+tag+splashPad, width)
			}
			rows = append(rows, edge.Render("│")+row+edge.Render("│"))
		}
	}
	rows = append(rows, edge.Render("╰"+strings.Repeat("─", width)+"╯"))
	return strings.Join(rows, "\n")
}

// splashCompact is the fallback for a terminal too narrow for the frame: the
// same three messages with their verdicts as a plain list, fitted to width.
func splashCompact(cfg categoryPalette, width int) string {
	tagW := 0
	for _, s := range splashSamples {
		if w := lipgloss.Width(s.cat); w > tagW {
			tagW = w
		}
	}
	avail := width - 2 - tagW - 2

	lines := make([]string, 0, len(splashSamples))
	for _, s := range splashSamples {
		tag := cfg.style(s.cat).Render(padRight(s.cat, tagW))
		if avail < 8 {
			// Too narrow for the messages; the verdicts are what matter.
			lines = append(lines, "  "+tag)
			continue
		}
		body := lipgloss.NewStyle().Foreground(lipgloss.Color("245")).
			Render(truncate(s.body, avail))
		lines = append(lines, "  "+tag+"  "+body)
	}
	return strings.Join(lines, "\n")
}

// categoryPalette is the slice of the rule config the mark needs, so this file
// doesn't need to know about the whole classifier.
type categoryPalette interface {
	style(category string) lipgloss.Style
}

// rulesPalette adapts a classify.Config.
type rulesPalette struct {
	colors   map[string]string
	fallback string
}

func (p rulesPalette) style(category string) lipgloss.Style {
	st := lipgloss.NewStyle().Bold(true)
	if c := p.colors[category]; c != "" {
		st = st.Foreground(lipgloss.Color(c))
	} else {
		st = st.Foreground(lipgloss.Color(p.fallback))
	}
	return st
}

// wrapBody breaks text to a cell width, on word boundaries where it can. A
// word longer than the width is split rather than allowed to overflow.
func wrapBody(s string, width int) []string {
	if width < 1 {
		return []string{""}
	}
	var out []string
	line := ""
	flush := func() {
		if line != "" {
			out = append(out, line)
			line = ""
		}
	}
	for _, word := range strings.Fields(s) {
		w := lipgloss.Width(word)
		switch {
		case line == "":
			line = word
		case lipgloss.Width(line)+1+w <= width:
			line += " " + word
		default:
			flush()
			line = word
		}
		// A single word too long for the row has to be cut somewhere.
		for lipgloss.Width(line) > width {
			r := []rune(line)
			cut := len(r)
			for cut > 1 && lipgloss.Width(string(r[:cut])) > width {
				cut--
			}
			out = append(out, string(r[:cut]))
			line = string(r[cut:])
		}
	}
	flush()
	if len(out) == 0 {
		out = []string{""}
	}
	return out
}

// padRight pads to a visible width, ignoring the escape sequences styling adds.
// padLeft indents so the text ends at width.
func padLeft(s string, width int) string {
	if gap := width - lipgloss.Width(s); gap > 0 {
		return strings.Repeat(" ", gap) + s
	}
	return s
}

func padRight(s string, width int) string {
	if gap := width - lipgloss.Width(s); gap > 0 {
		return s + strings.Repeat(" ", gap)
	}
	return s
}

// deviceView is the first screen: the launch mark above the device picker.
// Coming back to the picker from a category skips the mark — by then the user
// has seen it and is navigating back, not arriving.
func (m *Model) deviceView() string {
	if !m.showMarkOverPicker() {
		return m.deviceList.View()
	}
	return m.splashView() + "\n\n" + m.deviceList.View()
}

// showMark reports whether the launch mark is still wanted. It stays up for the
// first arrival at the device picker and the first sync, then never again.
func (m *Model) showMark() bool { return !m.synced }

// showMarkOverPicker is showMark with room for it. The picker is the thing you
// have to interact with, so on a short terminal the mark gives way.
func (m *Model) showMarkOverPicker() bool {
	return m.showMark() && m.height >= splashMinHeight
}

// markHeight is how many rows the mark occupies at the current width, so the
// list below it can be sized to what is left.
func (m *Model) markHeight() int {
	if !m.showMarkOverPicker() {
		return 0
	}
	return strings.Count(m.splashView(), "\n") + 1
}

// --- the category panel -------------------------------------------------------

// eighths are the sub-cell bar glyphs, so a category with two messages still
// shows a sliver instead of vanishing next to one with five thousand.
var eighths = []rune("▏▎▍▌▋▊▉█")

// panelRows is how many categories get a bar; the rest are summarised, since
// the panel is a shape, not a table — the list below is the table.
const panelRows = 6

// bar renders a bar `cells` wide for a fraction, with at least a sliver for
// anything non-zero.
func bar(frac float64, cells int) string {
	if cells <= 0 {
		return ""
	}
	if frac > 1 {
		frac = 1
	}
	if frac <= 0 {
		return ""
	}
	steps := int(frac*float64(cells)*8 + 0.5)
	if steps < 1 {
		steps = 1
	}
	full, rem := steps/8, steps%8

	var b strings.Builder
	for i := 0; i < full && i < cells; i++ {
		b.WriteRune(eighths[len(eighths)-1])
	}
	if full < cells && rem > 0 {
		b.WriteRune(eighths[rem-1])
	}
	return b.String()
}

// categoryPanel draws the launch frame filled with the store's real shape: a
// bar per category, in that category's own colour. It answers "what is this
// inbox made of?" before you pick, which is what this screen is for.
func categoryPanel(cfg categoryPalette, buckets []store.Bucket, total int, device string, width int) string {
	if width < splashFrameMin || total == 0 {
		return ""
	}
	edge := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	const pad = "  "

	live := make([]store.Bucket, 0, len(buckets))
	for _, b := range buckets {
		if b.Count > 0 {
			live = append(live, b)
		}
	}
	if len(live) == 0 {
		return ""
	}
	// Biggest first, so the shape reads top-down; ties keep rule order.
	sort.SliceStable(live, func(i, j int) bool { return live[i].Count > live[j].Count })
	shown := live
	if len(shown) > panelRows {
		shown = shown[:panelRows]
	}
	hidden := live[len(shown):]
	hiddenCount := 0
	for _, b := range hidden {
		hiddenCount += b.Count
	}

	max := shown[0].Count
	// Reserve the widest name and the widest count, then give the rest to the
	// bar so the column lines up regardless of the category names.
	nameW, countW := 0, 0
	for _, b := range shown {
		if w := lipgloss.Width(b.Category); w > nameW {
			nameW = w
		}
		if w := len(commify(b.Count)); w > countW {
			countW = w
		}
	}
	barW := width - 2*len(pad) - countW - 2 - nameW - 2
	if barW < 4 {
		return ""
	}

	rows := []string{edge.Render("╭" + strings.Repeat("─", width) + "╮")}
	head := fmt.Sprintf("%s messages · %s", commify(total), truncate(device, width-2*len(pad)-14))
	rows = append(rows, edge.Render("│")+
		padRight(pad+dimStyle.Render(truncate(head, width-2*len(pad))), width)+edge.Render("│"))

	for _, b := range shown {
		line := pad + cfg.style(b.Category).Render(padRight(b.Category, nameW)) + "  " +
			cfg.style(b.Category).Render(padRight(bar(float64(b.Count)/float64(max), barW), barW)) + "  " +
			dimStyle.Render(padLeft(commify(b.Count), countW))
		rows = append(rows, edge.Render("│")+padRight(line, width)+edge.Render("│"))
	}
	if hiddenCount > 0 {
		more := fmt.Sprintf("%d more · %s", len(hidden), commify(hiddenCount))
		rows = append(rows, edge.Render("│")+
			padRight(pad+dimStyle.Render(more), width)+edge.Render("│"))
	}
	rows = append(rows, edge.Render("╰"+strings.Repeat("─", width)+"╯"))
	return strings.Join(rows, "\n")
}

// commify adds thousands separators, so a five-figure count lines up with the
// rest of the column instead of shoving it out of alignment.
func commify(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	return s + "," + strings.Join(parts, ",")
}

// categoryStage is the panel above the category picker: the same launch frame,
// filled with the store's real shape rather than an example.
func (m *Model) categoryStage() string {
	parts := make([]string, 0, 3)
	if panel := m.categoryPanelView(); panel != "" {
		parts = append(parts, panel, "")
	}
	parts = append(parts, m.catList.View(), m.footer(m.catFooter()))
	return strings.Join(parts, "\n")
}

func (m *Model) categoryPanelView() string {
	if m.panel == "" || m.st == nil || m.width == 0 {
		return ""
	}
	w := m.width - 4
	if w > 64 {
		w = 64
	}
	pal := rulesPalette{colors: m.opt.Rules.Colors, fallback: "245"}
	return categoryPanel(pal, m.st.Buckets(m.opt.Rules), len(m.st.Records), m.st.Serial, w)
}

// refreshPanel caches the panel, so a resize doesn't rescan every message.
func (m *Model) refreshPanel() { m.panel = m.buildPanel() }

// buildPanel counts the store once. Called when the verdicts change, not when
// the terminal does.
func (m *Model) buildPanel() string {
	if m.st == nil {
		return ""
	}
	w := m.width - 4
	if w < splashFrameMin {
		return ""
	}
	if w > 64 {
		w = 64
	}
	pal := rulesPalette{colors: m.opt.Rules.Colors, fallback: "245"}
	return categoryPanel(pal, m.st.Buckets(m.opt.Rules), len(m.st.Records), m.st.Serial, w)
}

// panelHeight is how many rows the category panel occupies, so the list under
// it can be sized to what is left.
func (m *Model) panelHeight() int {
	if m.stage != stageCategory || m.st == nil {
		return 0
	}
	panel := m.categoryPanelView()
	if panel == "" {
		return 0
	}
	return strings.Count(panel, "\n") + 1
}
