package store

import "github.com/senorMk/android-text-classifier/internal/android"

// exportRecord adds explicit participants without changing the cached record.
type exportRecord struct {
	Record
	Sender   string `json:"sender"`
	Receiver string `json:"receiver"`
}

func forExport(r Record) exportRecord {
	contact := r.Sender()
	if contact == "" {
		contact = "Unknown contact"
	} else if r.Person != "" && r.Address != "" && r.Person != r.Address {
		contact += " (" + r.Address + ")"
	}
	sender, receiver := "Unknown sender", "Unknown receiver"
	switch r.Type {
	case android.KindInbox:
		sender, receiver = contact, "You"
	case android.KindSent, android.KindDraft, android.KindOutbox, android.KindFailed, android.KindQueued:
		sender, receiver = "You", contact
	}
	return exportRecord{Record: r, Sender: sender, Receiver: receiver}
}
