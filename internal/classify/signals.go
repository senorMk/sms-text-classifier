package classify

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/senorMk/android-text-classifier/internal/android"
)

// SenderKind is the shape of a message's originating address, which separates
// "a machine talking to you" from "someone you know".
type SenderKind int

const (
	SenderUnknown SenderKind = iota
	SenderShortCode
	SenderLongNumber
	SenderAlphanumeric // a registered sender id, e.g. "Google" or "VERIFY"
	SenderEmail
)

var (
	shortCodeRe  = regexp.MustCompile(`^\+?\d{3,6}$`)
	longNumberRe = regexp.MustCompile(`^\+?\d{7,15}$`)
	alphaNumRe   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9 &'\-_.]{0,20}$`)
)

// KindOfSender classifies an address. It is deliberately shape-based: the rules
// decide what a given shape *means*, which keeps the interpretation editable
// in rules.toml instead of baked in here.
func KindOfSender(address string) SenderKind {
	a := strings.TrimSpace(address)
	switch {
	case a == "":
		return SenderUnknown
	case strings.Contains(a, "@"):
		return SenderEmail
	case shortCodeRe.MatchString(a):
		return SenderShortCode
	case longNumberRe.MatchString(a):
		return SenderLongNumber
	case alphaNumRe.MatchString(a):
		return SenderAlphanumeric
	}
	return SenderUnknown
}

// Name maps a sender kind to the token used in rules.toml sender_kinds.
func (k SenderKind) Name() string {
	switch k {
	case SenderShortCode:
		return "shortcode"
	case SenderLongNumber:
		return "longnumber"
	case SenderAlphanumeric:
		return "alphanumeric"
	case SenderEmail:
		return "email"
	}
	return "unknown"
}

// Corpus holds the whole-message facts that no single row can tell you.
// Building it once per store is what lets a message be judged by its
// conversation rather than its text alone.
type Corpus struct {
	// Threads whose id appears on a message the user sent themselves — the
	// single strongest "this is a person, not a bot" signal.
	Replied map[int64]bool
	// Messages received per address, for spotting bulk senders.
	Received map[string]int
	// Messages the user sent per address, per their own person/address.
	Sent map[string]int
	// Bodies seen from an address, so a template shared by hundreds of
	// messages is recognizable.
	templates map[string]map[string]int
}

// NewCorpus indexes a set of messages. A nil or empty set yields an empty
// (but usable) corpus, so classification never has to nil-check it.
func NewCorpus(msgs []android.Message) *Corpus {
	c := &Corpus{
		Replied:   make(map[int64]bool),
		Received:  make(map[string]int),
		Sent:      make(map[string]int),
		templates: make(map[string]map[string]int),
	}
	for _, m := range msgs {
		if m.Type == android.KindInbox {
			c.Received[m.Address]++
		} else {
			c.Sent[m.Address]++
			c.Replied[m.ThreadID] = true
		}
		if m.Type != android.KindInbox {
			continue
		}
		key := templateKey(m.Body)
		if c.templates[m.Address] == nil {
			c.templates[m.Address] = make(map[string]int)
		}
		c.templates[m.Address][key]++
	}
	return c
}

// templateKey normalizes a body to its shape — lowercased, digits erased,
// whitespace collapsed — so "Order 4412 shipped" and "Order 9931 shipped"
// collapse to one key while genuinely different sentences stay apart.
func templateKey(body string) string {
	var b strings.Builder
	b.Grow(len(body))
	space, inDigits := false, false
	for _, r := range strings.ToLower(body) {
		switch {
		case unicode.IsDigit(r):
			// One placeholder per run of digits, so a 4-digit and a 6-digit
			// amount are still the same template.
			if !inDigits {
				if space && b.Len() > 0 {
					b.WriteByte(' ')
				}
				b.WriteByte('0')
			}
			space, inDigits = false, true
		case unicode.IsSpace(r):
			space, inDigits = true, false
		default:
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space, inDigits = false, false
			b.WriteRune(r)
		}
	}
	return b.String()
}

// IsBulk reports whether an address looks automated because it mostly repeats
// itself. threshold is how many identical templates are needed before the
// share is considered, and the dominant template has to also be at least half
// the address's traffic — otherwise a contact who texts "Okay" eleven times
// over four hundred messages would read as a marketing blast.
func (c *Corpus) IsBulk(address string, threshold int) bool {
	if threshold <= 0 {
		threshold = 5
	}
	total := c.Received[address]
	best := 0
	for _, n := range c.templates[address] {
		if n > best {
			best = n
		}
	}
	if best < threshold || total == 0 {
		return false
	}
	return float64(best)/float64(total) >= 0.5
}

// Inbound reports whether the message arrived from someone else.
func Inbound(m android.Message) bool { return m.Type == android.KindInbox }
