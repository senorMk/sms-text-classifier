package classify

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/senorMk/android-text-classifier/internal/android"
)

func msg(addr, body string) android.Message {
	return android.Message{Address: addr, Body: body, Type: android.KindInbox, ThreadID: 1}
}

// writeRules drops a rules.toml fragment in a temp dir and returns its path.
func writeRules(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "rules.toml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaultRulesLoad(t *testing.T) {
	cfg := Default()
	if len(cfg.Rules) == 0 {
		t.Fatal("no default rules loaded")
	}
	if cfg.Fallback != "other" {
		t.Errorf("fallback = %q, want other", cfg.Fallback)
	}
	// Every category a rule names must be listed in [ui].order, or it would be
	// classified but never shown.
	ordered := map[string]bool{}
	for _, c := range cfg.Order {
		ordered[c] = true
	}
	for _, r := range cfg.Rules {
		if !ordered[r.Category] {
			t.Errorf("category %q is used by rule %q but missing from ui.order", r.Category, r.name())
		}
	}
	for _, want := range []string{"otp", "banking", "delivery", "travel", "alerts", "work", "personal", "promo", "spam", "other"} {
		if !ordered[want] {
			t.Errorf("ui.order missing %q", want)
		}
	}
}

func TestClassify(t *testing.T) {
	cfg := Default()
	corpus := NewCorpus([]android.Message{
		msg("Google", "G-538204 is your Google verification code."),
		msg("FNB", "FNB:-) ZK64.00 reserved for purchase at Jara Food. Avail ZK20774."),
		msg("+260977123456", "are you coming tonight?"),
		{Address: "+260977123456", Body: "yes 7pm", Type: android.KindSent, ThreadID: 1},
		msg("UNKNOWN-1", "Congratulations! You have won a lottery prize. Claim now www.prize.xyz"),
		msg("SHOPZ", "50% OFF everything today only! Use code SAVE50. Unsubscribe"),
		msg("DELIVERYCO", "Your order #8891 has shipped. Track: https://dhl.com/x"),
		msg("Airtel", "Your airtime balance is K1107.9098. Valid for 30 days."),
	})

	cases := []struct {
		name, addr, body string
		want             string
	}{
		{"device otp flag", "Google", "G-538204 is your Google verification code. Don't share your code with anyone.", "otp"},
		{"bank alert", "FNB", "FNB:-) ZK64.00 reserved for purchase at Jara Food. Avail ZK20774.", "banking"},
		{"two-way chat", "+260977123456", "are you coming tonight?", "personal"},
		{"scam", "UNKNOWN-1", "Congratulations! You have won a lottery prize. Claim now www.prize.xyz", "spam"},
		{"marketing", "SHOPZ", "50% OFF everything today only! Use code SAVE50. Unsubscribe", "promo"},
		{"parcel", "DELIVERYCO", "Your order #8891 has shipped. Track: https://dhl.com/x", "delivery"},
		{"telecom", "Airtel", "Your airtime balance is K1107.9098. Valid for 30 days.", "alerts"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := c
			if m.body == "" {
				m.body = c.body
			}
			got := cfg.Classify(msg(c.addr, c.body), corpus)
			if got.Category != c.want {
				t.Errorf("classify(%q) = %q (%.2f, %s), want %q",
					c.addr, got.Category, got.Confidence, got.Reason, c.want)
			}
		})
	}
}

// The device's own retriever is far more reliable than any regex, so it must
// win outright even when a bank keyword is also present.
func TestContainsOTPOutweighsBodyRules(t *testing.T) {
	cfg := Default()
	m := msg("SomeBank", "Your verification code is 448201. Balance: 100 USD")
	m.ContainsOTP = true
	got := cfg.Classify(m, NewCorpus(nil))
	if got.Category != "otp" {
		t.Errorf("category = %q (%s), want otp", got.Category, got.Reason)
	}
	if got.NeedsReview {
		t.Error("a device-flagged OTP should not be flagged for review")
	}
}

// The confidence mechanic, tested against a fixed rule set so it stays about
// the scoring math rather than whatever the shipped defaults happen to say.
func TestConfidenceNeedsAgreement(t *testing.T) {
	mini, err := Load(writeRules(t, `
[engine]
strong = 2.0
review_below = 0.55

[[rule]]
category = "alpha"
label = "alpha keyword"
weight = 1.0
body_contains = ["alpha"]

[[rule]]
category = "alpha"
label = "second alpha signal"
weight = 1.0
body_contains = ["alphaword"]

[[rule]]
category = "beta"
label = "beta keyword"
weight = 1.0
body_contains = ["beta"]

[[rule]]
category = "beta"
label = "second beta signal"
weight = 1.0
body_contains = ["betalong"]

[[rule]]
category = "gamma"
label = "gamma keyword"
weight = 1.0
body_contains = ["gamma"]
`))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("one rule alone is a hint", func(t *testing.T) {
		// Repeating a keyword does not add weight — within a rule, entries
		// are alternatives. Only a second rule does that.
		got := mini.Classify(msg("+260977000111", "alpha alpha alpha"), nil)
		if got.Category != "alpha" {
			t.Errorf("category = %q, want alpha", got.Category)
		}
		// strength 0.5, nothing to beat -> 0.5, below the 0.55 bar.
		if !got.NeedsReview {
			t.Errorf("confidence %.2f, want review", got.Confidence)
		}
	})

	t.Run("two agreeing rules are a decision", func(t *testing.T) {
		got := mini.Classify(msg("+260977000111", "alpha and alphaword"), nil)
		if got.Category != "alpha" || got.NeedsReview {
			t.Errorf("alpha twice = %q (%.2f), want a confident alpha", got.Category, got.Confidence)
		}
	})

	t.Run("a coin flip is not a decision", func(t *testing.T) {
		got := mini.Classify(msg("+260977000111", "alpha alphaword beta betalong"), nil)
		if got.Category != "alpha" { // ties break on name
			t.Errorf("category = %q, want the tie to break alphabetically", got.Category)
		}
		if !got.NeedsReview {
			t.Errorf("a 2-2 tie at confidence %.2f must be flagged", got.Confidence)
		}
	})

	t.Run("beating the runner-up counts", func(t *testing.T) {
		got := mini.Classify(msg("+260977000111", "alpha alphaword beta"), nil)
		if got.Category != "alpha" || got.NeedsReview {
			t.Errorf("2-1 = %q (%.2f), want a confident alpha", got.Category, got.Confidence)
		}
	})
}

// A decisive rule settles a categorical fact instead of trading weight with a
// category the message merely also happens to match.
func TestDecisiveRuleWinsOutright(t *testing.T) {
	mini, err := Load(writeRules(t, `
[engine]
review_below = 0.55

[[rule]]
category = "otp"
label = "device flagged a code"
weight = 2.0
decisive = true
contains_otp = true

[[rule]]
category = "banking"
label = "bank wording"
weight = 3.0
body_contains = ["balance"]
`))
	if err != nil {
		t.Fatal(err)
	}
	m := msg("SomeBank", "Your verification code is 448201. Balance: 100 USD")
	m.ContainsOTP = true

	got := mini.Classify(m, nil)
	if got.Category != "otp" {
		t.Errorf("category = %q, want otp despite the heavier banking rule", got.Category)
	}
	if got.NeedsReview || got.Confidence != 1 {
		t.Errorf("confidence %.2f review %v, want a settled verdict", got.Confidence, got.NeedsReview)
	}
	if got.Reason != "device flagged a code" {
		t.Errorf("reason = %q", got.Reason)
	}

	// Without the flag the heavy rule is back in charge.
	m.ContainsOTP = false
	if got := mini.Classify(m, nil); got.Category != "banking" {
		t.Errorf("unflagged = %q, want banking", got.Category)
	}
}

func TestNothingMatchesFallsBackAndFlagsReview(t *testing.T) {
	cfg := Default()
	corpus := NewCorpus(nil)

	// An alphanumeric sender with no recognizable language reaches only the
	// low-weight "other" catch-all.
	got := cfg.Classify(msg("UNKNOWNX", strings.Repeat("z", 400)), corpus)
	if got.Category != "other" {
		t.Errorf("category = %q, want other", got.Category)
	}
	if !got.NeedsReview {
		t.Errorf("an unmatched message must be flagged for review (%.2f)", got.Confidence)
	}
	if !strings.Contains(got.Reason, "nothing matched") {
		t.Errorf("reason = %q, want the catch-all to explain itself", got.Reason)
	}

	// A two-letter reply from an unknown long number reaches no rule at all.
	got = cfg.Classify(msg("+260977000111", "ok"), corpus)
	if got.Category != "other" || !got.NeedsReview {
		t.Errorf("unclaimed short reply = %q (%.2f), want other + review", got.Category, got.Confidence)
	}
	if got.Reason != "no rule matched" {
		t.Errorf("reason = %q, want %q", got.Reason, "no rule matched")
	}
}

// Banks and couriers text from long numbers too, so the broad "known contact"
// rule has to step aside for anything that reads like machine output.
func TestKnownContactYieldsToTransactionalLanguage(t *testing.T) {
	cfg := Default()
	corpus := NewCorpus(nil)
	bank := msg("+260971234567", "Your available balance is ZK120.55 as of today, statement ready to view")
	if got := cfg.Classify(bank, corpus); got.Category == "personal" {
		t.Errorf("bank alert from a long number was claimed as personal (%s)", got.Reason)
	}
	person := msg("+260971234567", "I just landed, heading over to your place in about twenty minutes")
	if got := cfg.Classify(person, corpus); got.Category != "personal" {
		t.Errorf("a real message from a contact = %q (%s), want personal", got.Category, got.Reason)
	}
}

func TestKindOfSender(t *testing.T) {
	cases := map[string]SenderKind{
		"+260977123456":     SenderLongNumber,
		"260977123456":      SenderLongNumber,
		"+9182":             SenderShortCode,
		"12345":             SenderShortCode,
		"Google":            SenderAlphanumeric,
		"My Bank":           SenderAlphanumeric,
		"a@b.com":           SenderEmail,
		"":                  SenderUnknown,
		"+1 (555) 010-9999": SenderUnknown,
	}
	for addr, want := range cases {
		if got := KindOfSender(addr); got != want {
			t.Errorf("KindOfSender(%q) = %v (%s), want %v", addr, got, got.Name(), want)
		}
	}
}

func TestIsBulk(t *testing.T) {
	var msgs []android.Message
	for i := 0; i < 6; i++ {
		msgs = append(msgs, msg("BULK1", "Sale ends soon, order now, offer 9911"))
	}
	msgs = append(msgs, msg("CHAT1", "hey how are you doing today"))
	c := NewCorpus(msgs)
	if !c.IsBulk("BULK1", 5) {
		t.Error("6 identical-template messages should mark BULK1 as bulk")
	}
	if c.IsBulk("CHAT1", 5) {
		t.Error("a varied human sender should not be bulk")
	}
}

// A bulk sender must lose to a two-way conversation, or a newsletter thread the
// user ever replied to would read as personal.
func TestBulkLosesToRepliedThread(t *testing.T) {
	cfg := Default()
	msgs := []android.Message{
		msg("NEWS1", "50% off your cart, use code SAVE20, ends tonight"),
		{Address: "+260977000111", Body: "stop", Type: android.KindSent, ThreadID: 7},
	}
	c := NewCorpus(msgs)
	got := cfg.Classify(msgs[0], c)
	if got.Category == "personal" {
		t.Errorf("bulk sender classified as personal (%s)", got.Reason)
	}
}

func TestLoadRejectsBadRules(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"bad regex": `[[rule]]
category = "x"
body_regex = ['(unclosed']
`,
		"no category": `[[rule]]
body_contains = ["hi"]
`,
		"no conditions": `[[rule]]
category = "x"
weight = 2
`,
		"bad sender kind": `[[rule]]
category = "x"
sender_kinds = ["telepathy"]
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(dir, "rules.toml")
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(p); err == nil {
				t.Error("expected an error, got nil")
			}
		})
	}
}

func TestLoadMissingFileIsAnError(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.toml")); err == nil {
		t.Error("an explicit but missing --rules path must not silently use defaults")
	}
}

func TestLoadFirstFallsBackToBuiltIn(t *testing.T) {
	dir := t.TempDir()
	got, err := LoadFirst([]string{filepath.Join(dir, "nope.toml")})
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != "<built-in>" {
		t.Errorf("source = %q, want <built-in>", got.Source)
	}
}

func TestLoadFirstPicksFirstExisting(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "rules.toml")
	// No [ui].order, so the display order has to be derived from rule weight.
	body := "[engine]\nfallback = \"elsewhere\"\n\n[[rule]]\ncategory = \"a\"\nweight = 3\nbody_contains = [\"x\"]\n\n[[rule]]\ncategory = \"b\"\nbody_contains = [\"y\"]\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFirst([]string{filepath.Join(dir, "missing.toml"), p})
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != p || got.Fallback != "elsewhere" {
		t.Errorf("source = %q fallback = %q", got.Source, got.Fallback)
	}
	if len(got.Order) != 2 || got.Order[0] != "a" {
		t.Errorf("derived order = %v, want [a b] (heaviest first)", got.Order)
	}
}

func TestWriteTemplate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "rules.toml")
	if err := WriteTemplate(p); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err != nil {
		t.Errorf("written template does not load: %v", err)
	}
	if err := WriteTemplate(p); err == nil {
		t.Error("expected an error when overwriting an existing template")
	}
}

// A contact who texts "Okay" repeatedly over hundreds of messages is a person;
// a sender whose traffic is mostly one template is not.
func TestIsBulkNeedsDominance(t *testing.T) {
	chatty := []string{
		"are you up", "on my way", "call me when you land", "running late sorry",
		"did you eat", "yes good", "see you at seven", "bringing the kids",
		"ok", "sure thing", "need anything", "no thanks", "talk later",
		"gate is c12", "landing now", "where are we meeting", "at the usual place",
	}
	family := append([]string{}, chatty...)
	for i := 0; i < 11; i++ {
		family = append(family, "Okay ")
	}
	// Pad the family thread with varied traffic so "Okay" is a minority of it.
	family = append(family, chatty...)

	var msgs []android.Message
	for i := 0; i < 6; i++ {
		msgs = append(msgs, msg("BLAST", "Sale ends soon, order now, offer 9911"))
	}
	for _, b := range chatty {
		msgs = append(msgs, msg("CHATTY", b))
	}
	for _, b := range family {
		msgs = append(msgs, msg("FAMILY", b))
	}
	c := NewCorpus(msgs)

	if !c.IsBulk("BLAST", 5) {
		t.Error("an address whose traffic is one template should be bulk")
	}
	if c.IsBulk("CHATTY", 5) {
		t.Error("varied traffic should not be bulk")
	}
	if c.IsBulk("FAMILY", 5) {
		t.Errorf("a repeated line inside mostly-varied traffic is a person, not a blast (best of %d over %d)", bestTemplate(c, "FAMILY"), c.Received["FAMILY"])
	}
	if c.IsBulk("NOBODY", 5) {
		t.Error("an unknown address should not be bulk")
	}
}

// Numbers are deliberately erased from the template key, so "okay 1" and
// "okay 2" are the same template.
func TestTemplateKeyIgnoresDigits(t *testing.T) {
	a := templateKey("Your order 4412 shipped from store 7")
	b := templateKey("Your order 9931 shipped from store 2")
	if a != b {
		t.Errorf("templateKey() = %q vs %q, want equal", a, b)
	}
	if templateKey("are you up") == templateKey("on my way") {
		t.Error("different sentences should not collapse to one template")
	}
}

func bestTemplate(c *Corpus, addr string) int {
	best := 0
	for _, n := range c.templates[addr] {
		if n > best {
			best = n
		}
	}
	return best
}
