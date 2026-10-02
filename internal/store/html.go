package store

import (
	_ "embed"
	"encoding/json"
	"html/template"
	"io"
	"strconv"
	"strings"
	"time"
)

//go:embed export.html
var htmlTemplate string

// htmlTemplateOnce parses the embedded page. html/template is used rather than
// string building because message bodies are attacker-controlled text: a
// classification is only trustworthy if the export cannot be made to execute
// anything when opened.
var htmlTemplateOnce = template.Must(template.New("export").Parse(htmlTemplate))

// neutralColor is what categories without a colour of their own fall back to.
const neutralColor = "#7a8199"

// htmlRow is one pre-rendered table row.
type htmlRow struct {
	Record
	ThreadKey string
	JSON      string
	When      string
	Sender    string
	Address   string
	HasPerson bool
	Body      string
	Color     string
	RowClass  string
}

// htmlCategory is one filter chip.
type htmlCategory struct {
	Name  string
	Count int
	Color string
}

// htmlData is the whole page.
type htmlData struct {
	Title     string
	Serial    string
	Total     int
	First     string
	Last      string
	Rules     string
	Generated string
	Neutral   string
	Cats      []htmlCategory
	Records   []htmlRow
}

// ExportHTML writes records as a single self-contained page: inline CSS and
// JS, no external requests, and client-side filtering so a large export stays
// usable offline.
func ExportHTML(w io.Writer, records []Record, opts HTMLOptions) error {
	rows := make([]htmlRow, 0, len(records))
	counts := map[string]int{}
	order := append([]string(nil), opts.Order...)

	for _, r := range records {
		counts[r.Class.Category]++
		if counts[r.Class.Category] == 1 && !containsStr(order, r.Class.Category) {
			// A category the current rules don't list still has to be
			// reachable, or its messages would only be findable by search.
			order = append(order, r.Class.Category)
		}
		var classes []string
		if !r.Read {
			classes = append(classes, "unread")
		}
		if r.Class.Confidence < opts.ReviewBelow {
			classes = append(classes, "unsure")
		}
		if r.Class.Source != "" && r.Class.Source != SourceRules {
			classes = append(classes, "model")
		}
		data, err := json.Marshal(r)
		if err != nil {
			return err
		}
		rows = append(rows, htmlRow{
			ThreadKey: ThreadKey(r), JSON: string(data),
			Record:    r,
			When:      r.Date.Local().Format("2006-01-02 15:04"),
			Sender:    r.Sender(),
			Address:   r.Address,
			HasPerson: r.Person != "" && r.Person != r.Address,
			Body:      r.Body,
			Color:     colorFor(opts, r.Class.Category),
			RowClass:  strings.Join(classes, " "),
		})
	}

	cats := make([]htmlCategory, 0, len(order))
	for _, name := range order {
		if counts[name] == 0 {
			continue
		}
		cats = append(cats, htmlCategory{Name: name, Count: counts[name], Color: colorFor(opts, name)})
	}

	title := opts.Title
	if title == "" {
		title = "SMS messages"
	}
	data := htmlData{
		Title:     title,
		Serial:    opts.Serial,
		Total:     len(records),
		Generated: time.Now().Format("2 Jan 2006 15:04"),
		Rules:     opts.Rules,
		Neutral:   neutralColor,
		Cats:      cats,
		Records:   rows,
	}
	if len(records) > 0 {
		first, last := records[0].Date, records[0].Date
		for _, r := range records[1:] {
			if r.Date.Before(first) {
				first = r.Date
			}
			if r.Date.After(last) {
				last = r.Date
			}
		}
		data.First = first.Local().Format("2 Jan 2006")
		data.Last = last.Local().Format("2 Jan 2006")
	}
	return htmlTemplateOnce.Execute(w, data)
}

// HTMLOptions supplies the bits of context the page needs from the caller.
type HTMLOptions struct {
	Serial      string
	Title       string
	Rules       string
	Order       []string
	Colors      map[string]string
	ReviewBelow float64
}

// colorFor resolves a category's colour. The rules file uses ANSI numbers and
// the palette lookup has to come first, since a bare number is not a valid CSS
// colour. Anything unrecognised falls back to neutral rather than being passed
// through into a style attribute.
func colorFor(opts HTMLOptions, category string) string {
	c := opts.Colors[category]
	if c == "" {
		return neutralColor
	}
	if hex, ok := ansiToHex[c]; ok {
		return hex
	}
	if n, err := strconv.Atoi(c); err == nil {
		if hex, ok := ansiToHex[strconv.Itoa(n)]; ok {
			return hex
		}
	}
	if validColor(c) {
		return c
	}
	return neutralColor
}

// validColor accepts only #rgb / #rrggbb, so nothing else can escape the
// style attribute.
func validColor(c string) bool {
	if c == "" {
		return false
	}
	if !strings.HasPrefix(c, "#") {
		return false
	}
	rest := c[1:]
	if len(rest) != 3 && len(rest) != 6 {
		return false
	}
	for _, r := range rest {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}

// ansiToHex covers the 16 colours the default rules use, picked to stay
// legible on both the light and dark page styles.
var ansiToHex = map[string]string{
	"0": "#2b2f3a", "1": "#e05561", "2": "#5fb377", "3": "#c69026",
	"4": "#4f8bd6", "5": "#b06fd8", "6": "#3fa8a5", "7": "#9aa3b8",
	"8": "#6b7280", "9": "#ff6b7a", "10": "#7ed492", "11": "#e5b544",
	"12": "#6ba3ef", "13": "#cf8ff0", "14": "#5fd0cc", "15": "#c8cede",
	"39": "#7fc7ff", "45": "#6ee7b7", "86": "#5fd0cc", "111": "#7cc4ff",
	"150": "#63c38a", "170": "#e58ec4", "196": "#ff6b7a", "205": "#f4a3c0",
	"214": "#e5b544", "220": "#e5b544", "245": "#8a91a6",
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
