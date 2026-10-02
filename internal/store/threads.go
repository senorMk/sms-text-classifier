package store

import (
	"fmt"
	"sort"
	"strings"
)

// Thread is a conversation, with records ordered oldest first.
type Thread struct {
	Key     string
	Records []Record
}

// ThreadKey prefers the device's conversation identity. Missing IDs fall back
// to an exact address; anonymous messages must never collapse into one thread.
func ThreadKey(r Record) string {
	if r.ThreadID > 0 {
		return fmt.Sprintf("thread:%d", r.ThreadID)
	}
	if address := strings.TrimSpace(r.Address); address != "" {
		return "address:" + address
	}
	return fmt.Sprintf("message:%d", r.ID)
}

// Threads groups without changing the input and sorts conversations by latest
// activity, newest first. Message IDs break timestamp ties deterministically.
func Threads(records []Record) []Thread {
	groups := make(map[string][]Record)
	for _, r := range records {
		key := ThreadKey(r)
		groups[key] = append(groups[key], r)
	}
	threads := make([]Thread, 0, len(groups))
	for key, rows := range groups {
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Date.Equal(rows[j].Date) {
				return rows[i].ID < rows[j].ID
			}
			return rows[i].Date.Before(rows[j].Date)
		})
		threads = append(threads, Thread{Key: key, Records: rows})
	}
	sort.Slice(threads, func(i, j int) bool {
		a, b := threads[i].Latest(), threads[j].Latest()
		if a.Date.Equal(b.Date) {
			if a.ID == b.ID {
				return threads[i].Key < threads[j].Key
			}
			return a.ID > b.ID
		}
		return a.Date.After(b.Date)
	})
	return threads
}

func (t Thread) Latest() Record { return t.Records[len(t.Records)-1] }

func (t Thread) Label() string {
	if label := t.Latest().Sender(); strings.TrimSpace(label) != "" {
		return label
	}
	return "Unknown sender"
}
