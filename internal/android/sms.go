package android

import (
	"bufio"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Kind is the SmsProvider `type` column.
type Kind int

const (
	KindInbox   Kind = 1
	KindSent    Kind = 2
	KindDraft   Kind = 3
	KindOutbox  Kind = 4
	KindFailed  Kind = 5
	KindQueued  Kind = 6
	KindUnknown Kind = 0
)

// String renders the kind for the UI.
func (k Kind) String() string {
	switch k {
	case KindInbox:
		return "inbox"
	case KindSent:
		return "sent"
	case KindDraft:
		return "draft"
	case KindOutbox:
		return "outbox"
	case KindFailed:
		return "failed"
	case KindQueued:
		return "queued"
	}
	return "unknown"
}

// Message is one row of the device's SMS store.
type Message struct {
	ID            int64     `json:"id"`
	ThreadID      int64     `json:"thread_id"`
	Address       string    `json:"address"`
	Person        string    `json:"person,omitempty"`
	Date          time.Time `json:"date"`
	DateSent      time.Time `json:"date_sent,omitempty"`
	Type          Kind      `json:"type"`
	Read          bool      `json:"read"`
	Creator       string    `json:"creator,omitempty"`
	ContainsOTP   bool      `json:"contains_otp,omitempty"`
	ServiceCenter string    `json:"service_center,omitempty"`
	Body          string    `json:"body"`
}

// Sender returns the best display name for the message's origin: the resolved
// contact name when the messaging app knows one, else the raw address.
func (m Message) Sender() string {
	if m.Person != "" {
		return m.Person
	}
	return m.Address
}

// Store size knobs for the pull.

// projection lists the columns we request from the SMS provider.
//
// `body` must stay last: `content query` has no escaping, so a body containing
// ", creator=" or a newline would otherwise desync the field split. With body
// last, its value simply runs to the end of the record.
//
// `subject` is omitted on purpose — for MMS rows it is a protobuf blob, not
// text, and it only adds bytes to every pull.
var projection = []string{
	"_id",
	"thread_id",
	"address",
	"person",
	"date",
	"date_sent",
	"type",
	"read",
	"creator",
	"contains_otp",
	"service_center",
	"body",
}

// defaultSpan is the _id width of one `content query` round trip. The provider
// ignores LIMIT (the selection is wrapped in its own parens), so the only way
// to bound a pull is to page over _id ranges; a short page means the store is
// drained.
const defaultSpan = 20000

// PullOptions narrows a pull. The zero value pulls the whole store.
type PullOptions struct {
	AfterID  int64             // only rows with _id > AfterID (incremental sync)
	BeforeID int64             // only rows with _id <= BeforeID
	Since    time.Time         // only rows with date >= Since
	Limit    int               // stop after this many messages
	MaxID    int64             // only rows with _id <= MaxID
	Kinds    []Kind            // only these types (nil/empty = all)
	Span     int               // _id width per round trip (0 = defaultSpan)
	Progress func(fetched int) // called after each round trip
}

// Messages reads the SMS store off the device, newest-first ordering aside.
func (t *Toolchain) Messages(serial string, opt PullOptions) ([]Message, error) {
	span := opt.Span
	if span <= 0 {
		span = defaultSpan
	}

	var out []Message
	lo := opt.AfterID
	for {
		hi := lo + int64(span)
		if opt.MaxID > 0 && hi > opt.MaxID {
			hi = opt.MaxID
		}

		batch, err := t.pageMessages(serial, lo, hi, opt.Since, opt.Kinds)
		if err != nil {
			return nil, err
		}

		if opt.Limit > 0 && len(out)+len(batch) >= opt.Limit {
			batch = batch[:opt.Limit-len(out)]
			out = append(out, batch...)
			if opt.Progress != nil {
				opt.Progress(len(out))
			}
			return out, nil
		}
		out = append(out, batch...)
		if opt.Progress != nil {
			opt.Progress(len(out))
		}

		// A page smaller than the span means no more ids in range, and an
		// empty page is the normal end-of-store signal.
		if len(batch) < span {
			return out, nil
		}
		lo = hi
		if opt.MaxID > 0 && lo >= opt.MaxID {
			return out, nil
		}
	}
}

// pageMessages runs a single `content query` over the _id range (lo, hi] and
// parses the result.
func (t *Toolchain) pageMessages(serial string, lo, hi int64, since time.Time, kinds []Kind) ([]Message, error) {
	msgs, err := t.queryMessages(serial, lo, hi, since, kinds, projection)
	if err != nil && strings.Contains(err.Error(), "no such column: contains_otp") {
		// contains_otp is a vendor extension, absent on some phones.
		cols := make([]string, 0, len(projection)-1)
		for _, col := range projection {
			if col != "contains_otp" {
				cols = append(cols, col)
			}
		}
		return t.queryMessages(serial, lo, hi, since, kinds, cols)
	}
	return msgs, err
}

func (t *Toolchain) queryMessages(serial string, lo, hi int64, since time.Time, kinds []Kind, cols []string) ([]Message, error) {
	args := t.deviceArgs(serial, "shell", "content", "query",
		"--uri", "content://sms",
		"--projection", strings.Join(cols, ":"),
	)
	if where := buildWhere(lo, hi, since, kinds); where != "" {
		// The quotes are part of the argument on purpose: adb joins its
		// arguments and the *device* shell re-parses them, so a bare
		// "_id>5" would be read as a redirection. Wrapping the clause in
		// single quotes survives the second parse.
		args = append(args, "--where", "'"+where+"'")
	}

	cmd := exec.Command(t.ADB, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("content query: %w", err)
	}

	msgs, err := parseQuery(stdout, cols)
	if err != nil {
		cmd.Wait()
		return nil, err
	}
	if err := cmd.Wait(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("content query: %s", msg)
	}
	// Some Android versions write the same provider error to stderr while
	// still returning exit status 0.
	if msg := strings.TrimSpace(stderr.String()); strings.Contains(msg, "Error while accessing provider:") {
		return nil, fmt.Errorf("content query: %s", msg)
	}
	return msgs, nil
}

// buildWhere assembles the SQL fragment passed to `--where`. The provider wraps
// whatever we hand it in parentheses, so it must contain no ORDER BY or LIMIT.
func buildWhere(lo, hi int64, since time.Time, kinds []Kind) string {
	var clauses []string
	if lo > 0 {
		clauses = append(clauses, fmt.Sprintf("_id>%d", lo))
	}
	if hi > 0 {
		clauses = append(clauses, fmt.Sprintf("_id<=%d", hi))
	}
	if !since.IsZero() {
		clauses = append(clauses, fmt.Sprintf("date>=%d", since.UnixMilli()))
	}
	if len(kinds) > 0 {
		parts := make([]string, 0, len(kinds))
		for _, k := range kinds {
			parts = append(parts, strconv.Itoa(int(k)))
		}
		clauses = append(clauses, "type IN ("+strings.Join(parts, ",")+")")
	}
	return strings.Join(clauses, " AND ")
}

// ParseQuery turns `content query` output into messages.
//
// The format is a flat `Row: <n> k=v, k=v…` listing, one row per line, with no
// escaping at all: bodies contain newlines, commas and even strings that look
// like `, creator=`. So the output is reassembled into logical records first —
// a line only starts a record when it passes every check in recordStart.
func ParseQuery(r io.Reader) ([]Message, error) {
	return parseQuery(r, projection)
}

func parseQuery(r io.Reader, cols []string) ([]Message, error) {
	sc := bufio.NewScanner(r)
	// Bodies can be long (MMS forwards, concatenated segments); 1 MiB per
	// line is a generous ceiling that still bounds a runaway allocation.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var (
		msgs          []Message
		cur           strings.Builder
		active        bool
		providerError strings.Builder
		want          int // index of the next row, per round trip
	)
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		if n, ok := recordStartColumns(line, want, cols); ok {
			if active {
				msgs = append(msgs, parseRecordColumns(cur.String(), cols))
			}
			cur.Reset()
			cur.WriteString(line[n:])
			active = true
			want++
			continue
		}
		if active {
			cur.WriteByte('\n')
			cur.WriteString(line)
		}
		// Android's content command can print provider errors on stdout
		// and exit successfully. Drain the pipe before reporting the error.
		if !active && (providerError.Len() > 0 || strings.HasPrefix(line, "Error while accessing provider:")) {
			providerError.WriteString(line)
			providerError.WriteByte('\n')
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if providerError.Len() > 0 {
		return nil, fmt.Errorf("content query: %s", strings.TrimSpace(providerError.String()))
	}
	if active {
		msgs = append(msgs, parseRecordColumns(cur.String(), cols))
	}
	return msgs, nil
}

var rowRe = regexp.MustCompile(`^Row: (\d+) ` + regexp.QuoteMeta(projection[0]) + `=`)

// recordStart reports whether line opens a new record, returning how much of it
// the row prefix occupies. Three things must hold, and it takes all three to
// survive a hostile body:
//
//   - the line carries the `Row: <n> _id=` prefix;
//   - <n> is the next index we expect (a round trip restarts the counter at 0);
//   - every remaining column delimiter appears, in order — a body line that
//     merely imitates a row won't have them all.
func recordStart(line string, want int) (int, bool) {
	return recordStartColumns(line, want, projection)
}

func recordStartColumns(line string, want int, cols []string) (int, bool) {
	m := rowRe.FindStringSubmatch(line)
	if m == nil {
		return 0, false
	}
	idx, err := strconv.Atoi(m[1])
	if err != nil || idx != want {
		return 0, false
	}
	rest := line[len(m[0]):]
	for _, col := range cols[1:] {
		delim := ", " + col + "="
		j := strings.Index(rest, delim)
		if j < 0 {
			return 0, false
		}
		rest = rest[j+len(delim):]
	}
	return len(m[0]), true
}

// parseRecord splits one reassembled record into its projected columns. The
// leading `Row: <n> ` is already gone; a lingering "_id=" prefix is tolerated
// so a stray caller can't silently zero out the id.
func parseRecord(rec string) Message {
	return parseRecordColumns(rec, projection)
}

func parseRecordColumns(rec string, cols []string) Message {
	rec = strings.TrimPrefix(rec, cols[0]+"=")
	vals := splitRecord(rec, cols)

	get := func(col string) string {
		for i, name := range cols {
			if name == col {
				return vals[i]
			}
		}
		return ""
	}
	num := func(col string) int64 {
		n, _ := strconv.ParseInt(strings.TrimSpace(get(col)), 10, 64)
		return n
	}
	ms := time.UnixMilli(num("date")).UTC()
	// A zero epoch means the column was NULL or unset.
	if num("date") == 0 {
		ms = time.Time{}
	}
	sent := time.Time{}
	if num("date_sent") != 0 {
		sent = time.UnixMilli(num("date_sent")).UTC()
	}

	return Message{
		ID:            num("_id"),
		ThreadID:      num("thread_id"),
		Address:       get("address"),
		Person:        nullString(get("person")),
		Date:          ms,
		DateSent:      sent,
		Type:          Kind(num("type")),
		Read:          num("read") != 0,
		Creator:       nullString(get("creator")),
		ContainsOTP:   num("contains_otp") != 0,
		ServiceCenter: nullString(get("service_center")),
		Body:          get("body"),
	}
}

// splitRecord walks the columns in order, cutting at each ", <next>="
// delimiter. Only the final column is unbounded, which is why `body` has to be
// last: any earlier column is still at the mercy of a value that happens to
// contain a comma followed by the next column's name.
func splitRecord(rec string, cols []string) []string {
	vals := make([]string, len(cols))
	cursor := 0
	for i := 0; i < len(cols)-1; i++ {
		delim := ", " + cols[i+1] + "="
		idx := strings.Index(rec[cursor:], delim)
		if idx < 0 {
			// Truncated row: everything left belongs to this column.
			vals[i] = strings.TrimSpace(rec[cursor:])
			return vals
		}
		vals[i] = strings.TrimSpace(rec[cursor : cursor+idx])
		cursor += idx + len(delim)
	}
	vals[len(cols)-1] = rec[cursor:]
	return vals
}

// nullString maps the provider's NULL sentinel to the empty string.
func nullString(s string) string {
	if s == "NULL" {
		return ""
	}
	return s
}
