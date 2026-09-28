package android

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMessagesWithoutOTPColumn(t *testing.T) {
	for _, stream := range []string{"stdout", "stderr"} {
		t.Run(stream, func(t *testing.T) {
			testMessagesWithoutOTPColumn(t, stream)
		})
	}
}

func testMessagesWithoutOTPColumn(t *testing.T, stream string) {
	adb := filepath.Join(t.TempDir(), "adb")
	// Android's content command reports SQL errors on stdout with exit 0.
	script := `#!/bin/sh
case "$*" in
  *contains_otp*)
    echo 'Error while accessing provider:sms'
    echo 'android.database.sqlite.SQLiteException: no such column: contains_otp'
    ;;
  *)
    echo 'Row: 0 _id=7, thread_id=3, address=Bank, person=NULL, date=1700000000000, date_sent=0, type=1, read=1, creator=x, service_center=NULL, body=hello, contains_otp=1'
    echo 'second line'
    echo 'Row: 1 _id=8, thread_id=3, address=Friend, person=NULL, date=1700000001000, date_sent=0, type=2, read=0, creator=x, service_center=+123, body=thanks'
    ;;
esac
`
	if stream == "stderr" {
		script = strings.ReplaceAll(script, "echo 'Error while accessing provider:sms'", "echo 'Error while accessing provider:sms' >&2")
		script = strings.ReplaceAll(script, "echo 'android.database.sqlite.SQLiteException: no such column: contains_otp'", "echo 'android.database.sqlite.SQLiteException: no such column: contains_otp' >&2")
	}
	if err := os.WriteFile(adb, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	got, err := (&Toolchain{ADB: adb}).Messages("test-device", PullOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2", len(got))
	}
	if got[0].ID != 7 || got[0].ContainsOTP || got[0].Body != "hello, contains_otp=1\nsecond line" {
		t.Fatalf("unexpected fallback record: %#v", got[0])
	}
	if got[1].ID != 8 || got[1].ServiceCenter != "+123" || got[1].Body != "thanks" {
		t.Fatalf("unexpected second record: %#v", got[1])
	}
}

func TestParseQueryProviderErrors(t *testing.T) {
	for _, detail := range []string{
		"android.database.sqlite.SQLiteException: no such column: contains_otp",
		"java.lang.SecurityException: Permission Denial",
	} {
		got, err := ParseQuery(strings.NewReader("Error while accessing provider:sms\n" + detail))
		if err == nil || !strings.Contains(err.Error(), detail) || len(got) != 0 {
			t.Fatalf("got %v, %v; want provider error %q", got, err, detail)
		}
	}
	got, err := ParseQuery(strings.NewReader("No result found.\n"))
	if err != nil || len(got) != 0 {
		t.Fatalf("empty store: got %v, %v", got, err)
	}
}

func TestParseQuery(t *testing.T) {
	out := `Row: 0 _id=16148, thread_id=92, address=Google, person=NULL, date=1790587291210, date_sent=1790587289000, type=1, read=1, creator=com.google.android.apps.messaging, contains_otp=1, service_center=+918299901900, body=G-538204 is your Google verification code. Don't share your code with anyone.
Row: 1 _id=16147, thread_id=92, address=+13622, person=Kalela, date=1790535755714, date_sent=0, type=2, read=0, creator=com.google.android.apps.messaging, contains_otp=0, service_center=NULL, body=Thanks 🙏 very much`
	got, err := ParseQuery(strings.NewReader(out))
	if err != nil {
		t.Fatalf("ParseQuery() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2", len(got))
	}

	first := got[0]
	if first.ID != 16148 || first.ThreadID != 92 {
		t.Errorf("ids = %d/%d, want 16148/92", first.ID, first.ThreadID)
	}
	if first.Address != "Google" || first.Person != "" {
		t.Errorf("address/person = %q/%q, want Google/\"\" (NULL)", first.Address, first.Person)
	}
	if !first.ContainsOTP {
		t.Error("contains_otp should be true")
	}
	if first.Type != KindInbox || !first.Read {
		t.Errorf("type/read = %v/%v, want inbox/true", first.Type, first.Read)
	}
	if first.ServiceCenter != "+918299901900" {
		t.Errorf("service_center = %q", first.ServiceCenter)
	}
	if want := time.UnixMilli(1790587291210).UTC(); !first.Date.Equal(want) {
		t.Errorf("date = %v, want %v", first.Date, want)
	}
	if !first.DateSent.Equal(time.UnixMilli(1790587289000).UTC()) {
		t.Errorf("date_sent = %v", first.DateSent)
	}

	second := got[1]
	if second.Person != "Kalela" || second.Sender() != "Kalela" {
		t.Errorf("person = %q, sender = %q", second.Person, second.Sender())
	}
	if !second.DateSent.IsZero() {
		t.Errorf("date_sent should be zero for 0, got %v", second.DateSent)
	}
	if second.Sender() != "Kalela" || got[0].Sender() != "Google" {
		t.Error("Sender() should fall back to the address when person is NULL")
	}
}

// Bodies are dumped raw, so they routinely contain newlines, commas, and text
// that looks exactly like the separator the parser splits on.
func TestParseQueryHostileBodies(t *testing.T) {
	out := `Row: 0 _id=5, thread_id=1, address=+15551234, person=NULL, date=1700000000000, date_sent=0, type=1, read=0, creator=com.google.android.apps.messaging, contains_otp=0, service_center=NULL, body=line one
Row: 1 _id=9, thread_id=1, address=+15551234, person=NULL, date=1700000000000, date_sent=0, type=1, read=0, creator=com.google.android.apps.messaging, contains_otp=0, service_center=NULL, body=line two, creator=injected, body=still mine
Row: 2 _id=10, thread_id=1, address=+15559999, person=NULL, date=1700000001000, date_sent=0, type=1, read=0, creator=com.google.android.apps.messaging, contains_otp=0, service_center=NULL, body=plain`
	got, err := ParseQuery(strings.NewReader(out))
	if err != nil {
		t.Fatalf("ParseQuery() error = %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d messages, want 3: %#v", len(got), got)
	}
	if got[0].Body != "line one" {
		t.Errorf("first body = %q", got[0].Body)
	}
	// The injected ", creator=injected, body=" must not have been read as
	// columns of its own.
	if want := "line two, creator=injected, body=still mine"; got[1].Body != want {
		t.Errorf("second body = %q, want %q", got[1].Body, want)
	}
	if got[1].Creator != "com.google.android.apps.messaging" {
		t.Errorf("second creator = %q", got[1].Creator)
	}
	if got[2].Body != "plain" || got[2].ID != 10 {
		t.Errorf("third = %#v, want id 10 body %q", got[2], "plain")
	}
}

func TestParseQuerySkipsJunk(t *testing.T) {
	// Unrelated text before any row is ignored; text
	// after the last row is indistinguishable from a body continuation, so it
	// stays with the record it follows.
	out := "unrelated startup noise\nRow: 0 _id=1, thread_id=1, address=+1, person=NULL, date=1, date_sent=0, type=1, read=0, creator=x, contains_otp=0, service_center=NULL, body=hi\ntrailing noise"
	got, err := ParseQuery(strings.NewReader(out))
	if err != nil {
		t.Fatalf("ParseQuery() error = %v", err)
	}
	if len(got) != 1 || got[0].Body != "hi\ntrailing noise" {
		t.Fatalf("got %#v, want a single message with body %q", got, "hi\ntrailing noise")
	}
}

func TestBuildWhere(t *testing.T) {
	cases := []struct {
		name   string
		lo, hi int64
		since  time.Time
		kinds  []Kind
		want   string
	}{
		{name: "empty", want: ""},
		{name: "range", lo: 10, hi: 20, want: "_id>10 AND _id<=20"},
		{name: "upper only", hi: 20, want: "_id<=20"},
		{name: "kinds", lo: 5, kinds: []Kind{KindInbox, KindSent}, want: "_id>5 AND type IN (1,2)"},
		{
			name:  "since",
			since: time.Unix(0, 1_700_000_000_000*int64(time.Millisecond)),
			want:  "date>=1700000000000",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := buildWhere(c.lo, c.hi, c.since, c.kinds); got != c.want {
				t.Errorf("buildWhere() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestParseRecordTruncated(t *testing.T) {
	m := parseRecord("_id=7, thread_id=3, address=+1555")
	if m.ID != 7 || m.ThreadID != 3 || m.Address != "+1555" {
		t.Errorf("parseRecord() = %#v", m)
	}
}

// A body that imitates a complete row, prefix and all, must not split records.
func TestParseQueryFullyImitatedRow(t *testing.T) {
	body := "Row: 1 _id=99, thread_id=9, address=+15550000, person=NULL, date=1, date_sent=0, type=1, read=0, creator=spoofed, contains_otp=0, service_center=NULL, body=fake"
	out := "Row: 0 _id=7, thread_id=1, address=+1, person=NULL, date=1, date_sent=0, type=1, read=0, creator=x, contains_otp=0, service_center=NULL, body=" + body + "\n" +
		"Row: 1 _id=8, thread_id=1, address=+1, person=NULL, date=2, date_sent=0, type=1, read=0, creator=x, contains_otp=0, service_center=NULL, body=real"
	got, err := ParseQuery(strings.NewReader(out))
	if err != nil {
		t.Fatalf("ParseQuery() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2", len(got))
	}
	if got[0].Body != body {
		t.Errorf("first body = %q, want %q", got[0].Body, body)
	}
	if got[1].Body != "real" || got[1].ID != 8 {
		t.Errorf("second = %#v, want id 8 body %q", got[1], "real")
	}
}

func TestKindString(t *testing.T) {
	for kind, want := range map[Kind]string{
		KindInbox: "inbox", KindSent: "sent", KindDraft: "draft",
		KindOutbox: "outbox", KindFailed: "failed", KindQueued: "queued",
		KindUnknown: "unknown", Kind(99): "unknown",
	} {
		if got := kind.String(); got != want {
			t.Errorf("Kind(%d).String() = %q, want %q", kind, got, want)
		}
	}
}
