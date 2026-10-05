package store

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"html"
	"regexp"
	"strings"
	"testing"

	"github.com/senorMk/android-text-classifier/internal/android"
)

func TestExportParticipants(t *testing.T) {
	for _, tc := range []struct {
		kind             android.Kind
		sender, receiver string
	}{
		{android.KindInbox, "Kalela (+260977000111)", "You"},
		{android.KindSent, "You", "Kalela (+260977000111)"},
		{android.KindDraft, "You", "Kalela (+260977000111)"},
		{android.KindOutbox, "You", "Kalela (+260977000111)"},
		{android.KindFailed, "You", "Kalela (+260977000111)"},
		{android.KindQueued, "You", "Kalela (+260977000111)"},
		{android.Kind(99), "Unknown sender", "Unknown receiver"},
	} {
		t.Run(tc.kind.String(), func(t *testing.T) {
			r := rec(9007199254740993, "reply", "personal", 1, "rules")
			r.Type = tc.kind
			var b bytes.Buffer
			if err := ExportJSON(&b, []Record{r}); err != nil {
				t.Fatal(err)
			}
			var rows []exportRecord
			if err := json.Unmarshal(b.Bytes(), &rows); err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].ID != r.ID || rows[0].Sender != tc.sender || rows[0].Receiver != tc.receiver {
				t.Fatalf("JSON participants: %+v", rows)
			}
			b.Reset()
			if err := ExportCSV(&b, []Record{r}); err != nil {
				t.Fatal(err)
			}
			csvRows, err := csv.NewReader(&b).ReadAll()
			if err != nil {
				t.Fatal(err)
			}
			for i, name := range csvRows[0] {
				if name == "sender" && csvRows[1][i] != tc.sender || name == "receiver" && csvRows[1][i] != tc.receiver {
					t.Fatalf("CSV %s: %q", name, csvRows[1][i])
				}
			}
			page := render(t, []Record{r})
			for _, want := range []string{`<td class="from">` + html.EscapeString(tc.sender) + `</td>`, `<td class="to">` + html.EscapeString(tc.receiver) + `</td>`} {
				if !strings.Contains(html.UnescapeString(page), html.UnescapeString(want)) {
					t.Fatalf("HTML missing %s", want)
				}
			}
			match := regexp.MustCompile(`data-record="([^"]*)"`).FindStringSubmatch(page)
			var embedded exportRecord
			if len(match) != 2 {
				t.Fatal("missing HTML record")
			}
			if err := json.Unmarshal([]byte(html.UnescapeString(match[1])), &embedded); err != nil {
				t.Fatal(err)
			}
			if embedded.Sender != tc.sender || embedded.Receiver != tc.receiver {
				t.Fatalf("thread participants: %+v", embedded)
			}
		})
	}
}

func TestExportContactFallbacks(t *testing.T) {
	r := rec(1, "body", "personal", 1, "rules")
	r.Person = ""
	if got := forExport(r).Sender; got != r.Address {
		t.Fatalf("address fallback: %q", got)
	}
	r.Address = ""
	if got := forExport(r).Sender; got != "Unknown contact" {
		t.Fatalf("anonymous contact: %q", got)
	}
	r.Person = "<img src=x onerror=alert(1)>"
	r.Type = android.KindSent
	page := render(t, []Record{r})
	if strings.Contains(page, "<img") || !strings.Contains(page, "&lt;img") {
		t.Fatal("receiver must remain escaped text")
	}
}
