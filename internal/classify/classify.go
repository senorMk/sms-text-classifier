// Package classify turns a message into a category using an editable set of
// rules (rules.toml) plus a few whole-store signals.
//
// Every rule contributes weight to one category; the heaviest category wins and
// the gap to the runner-up becomes the confidence. Low-confidence results are
// flagged for review, which is what the optional LLM pass consumes — so the
// deterministic engine stays the source of truth and the model is only ever a
// second opinion on the residue.
package classify

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/senorMk/android-text-classifier/internal/android"
)

//go:embed rules.default.toml
var defaultRules []byte

// DefaultRulesPath returns a human-visible location for the built-in template.
const DefaultRulesPath = "rules.toml"

// Rule is one editable rule.
//
// The Body*/Sender* pairs follow a deliberate split: the *Raw slices are what
// rules.toml decodes into, the *Is slices hold the compiled regexps. Keeping
// them apart means a bad pattern is reported when the file loads rather than
// on the first message that happens to reach it.
type Rule struct {
	Category string  `toml:"category"`
	Label    string  `toml:"label"`
	Weight   float64 `toml:"weight"`

	BodyHas    []string         `toml:"body_contains"`
	BodyRaw    []string         `toml:"body_regex"`
	BodyIs     []*regexp.Regexp `toml:"-"`
	NotBodyHas []string         `toml:"not_body_contains"`
	NotBodyRaw []string         `toml:"not_body_regex"`
	NotBodyIs  []*regexp.Regexp `toml:"-"`
	SenderHas  []string         `toml:"sender_contains"`
	SenderRaw  []string         `toml:"sender_regex"`
	SenderIs   []*regexp.Regexp `toml:"-"`

	// Decisive makes this rule win outright when it fires, ignoring total
	// weight. For categorical signals — the device's own OTP flag being the
	// obvious case — a near-tie with a category it merely happens to contain
	// is a false ambiguity.
	Decisive bool `toml:"decisive"`

	SenderKinds   []string `toml:"sender_kinds"`
	ContainsOTP   *bool    `toml:"contains_otp"`
	Inbound       *bool    `toml:"inbound"`
	ThreadReplied *bool    `toml:"thread_replied"`
	Bulk          *bool    `toml:"bulk"`
	MinLength     int      `toml:"min_length"`
	MaxLength     int      `toml:"max_length"`
}

// fileConfig mirrors the on-disk shape of rules.toml.
type fileConfig struct {
	Engine struct {
		Fallback     string  `toml:"fallback"`
		Strong       float64 `toml:"strong"`
		ReviewBelow  float64 `toml:"review_below"`
		BulkTemplate int     `toml:"bulk_template"`
	} `toml:"engine"`
	UI struct {
		Order []string          `toml:"order"`
		Color map[string]string `toml:"color"`
	} `toml:"ui"`
	Rules []Rule `toml:"rule"`
	// Categories carries the prose each category means, for the optional
	// model review pass. It is separate from the rules on purpose: a rule
	// says what to match, a description says what the name means, and the
	// two are edited for different reasons.
	Categories map[string]struct {
		Description string `toml:"description"`
	} `toml:"category"`
}

// Config is a validated, ready-to-use rule set.
type Config struct {
	Fallback     string
	Strong       float64
	ReviewBelow  float64
	BulkTemplate int
	Order        []string
	Colors       map[string]string
	Rules        []Rule
	// Descriptions explains each category in one precise sentence, for the
	// model review pass. Categories without one get a neutral fallback.
	Descriptions map[string]string
	Source       string // path it came from, or "<built-in>"

	raw []byte // verbatim file contents, for Fingerprint
}

// Fingerprint identifies the exact rule set, so a store can tell when its
// stored verdicts were produced by different rules. Editing rules.toml changes
// the hash and the store re-classifies on next open.
func (c *Config) Fingerprint() string {
	sum := sha256.Sum256(c.raw)
	return hex.EncodeToString(sum[:])[:16]
}

// Result is one message's verdict.
type Result struct {
	Category    string
	Score       float64
	Confidence  float64
	Reason      string
	NeedsReview bool
}

// Load reads a rule set from an explicit path. The file must exist — a typo in
// --rules is an error, not a silent fall back to the defaults.
func Load(path string) (*Config, error) {
	if path == "" {
		return Default(), nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parse(b, path)
}

// LoadFirst walks paths in order and loads the first rule file that exists,
// falling back to the embedded template. The file it picked is recorded in
// Config.Source so the UI can say which rules are in force.
func LoadFirst(paths []string) (*Config, error) {
	for _, p := range paths {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			continue
		}
		return Load(p)
	}
	return Default(), nil
}

// Default returns the embedded rule set.
func Default() *Config {
	cfg, err := parse(defaultRules, "<built-in>")
	if err != nil {
		// The template is compiled in; a failure here is a build-time bug,
		// not something a user can cause at runtime.
		panic("classify: built-in rules are invalid: " + err.Error())
	}
	return cfg
}

func parse(data []byte, source string) (*Config, error) {
	var fc fileConfig
	if _, err := toml.Decode(string(data), &fc); err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}

	cfg := &Config{
		Fallback:     fc.Engine.Fallback,
		Strong:       fc.Engine.Strong,
		ReviewBelow:  fc.Engine.ReviewBelow,
		BulkTemplate: fc.Engine.BulkTemplate,
		Order:        fc.UI.Order,
		Colors:       fc.UI.Color,
		Source:       source,
		Descriptions: map[string]string{},
		raw:          data,
	}
	if cfg.Fallback == "" {
		cfg.Fallback = "other"
	}
	if cfg.Strong <= 0 {
		cfg.Strong = 2.0
	}
	if cfg.ReviewBelow <= 0 {
		cfg.ReviewBelow = 0.55
	}
	if cfg.BulkTemplate <= 0 {
		cfg.BulkTemplate = 5
	}

	for i := range fc.Rules {
		r := fc.Rules[i]
		if strings.TrimSpace(r.Category) == "" {
			return nil, fmt.Errorf("%s: rule %d has no category", source, i+1)
		}
		if r.Weight == 0 {
			r.Weight = 1.0
		}
		var err error
		if r.BodyIs, err = compileAll(r.BodyRaw); err != nil {
			return nil, fmt.Errorf("%s: rule %q: body_regex: %w", source, r.name(), err)
		}
		if r.NotBodyIs, err = compileAll(r.NotBodyRaw); err != nil {
			return nil, fmt.Errorf("%s: rule %q: not_body_regex: %w", source, r.name(), err)
		}
		if r.SenderIs, err = compileAll(r.SenderRaw); err != nil {
			return nil, fmt.Errorf("%s: rule %q: sender_regex: %w", source, r.name(), err)
		}
		for _, k := range r.SenderKinds {
			if !validSenderKind(k) {
				return nil, fmt.Errorf("%s: rule %q: unknown sender_kind %q", source, r.name(), k)
			}
		}
		if !r.hasCondition() {
			return nil, fmt.Errorf("%s: rule %q has no conditions and would match everything", source, r.name())
		}
		cfg.Rules = append(cfg.Rules, r)
	}

	for name, c := range fc.Categories {
		if d := strings.TrimSpace(c.Description); d != "" {
			cfg.Descriptions[name] = d
		}
	}

	// Every category named by a rule must be reachable in the display order,
	// or it would be classified but never listed.
	if len(cfg.Order) == 0 {
		cfg.Order = cfg.categoriesByWeight()
	}
	return cfg, nil
}

func (r Rule) name() string {
	if r.Label != "" {
		return r.Label
	}
	return r.Category
}

func (r Rule) hasCondition() bool {
	return len(r.BodyHas) > 0 || len(r.BodyIs) > 0 || len(r.NotBodyHas) > 0 || len(r.NotBodyIs) > 0 ||
		len(r.SenderHas) > 0 || len(r.SenderIs) > 0 || len(r.SenderKinds) > 0 ||
		r.ContainsOTP != nil || r.Inbound != nil || r.ThreadReplied != nil ||
		r.Bulk != nil || r.MinLength > 0 || r.MaxLength > 0
}

func compileAll(exprs []string) ([]*regexp.Regexp, error) {
	if len(exprs) == 0 {
		return nil, nil
	}
	out := make([]*regexp.Regexp, 0, len(exprs))
	for _, e := range exprs {
		re, err := regexp.Compile(e)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", e, err)
		}
		out = append(out, re)
	}
	return out, nil
}

func validSenderKind(k string) bool {
	switch k {
	case "shortcode", "longnumber", "alphanumeric", "email", "unknown":
		return true
	}
	return false
}

func (c *Config) categoriesByWeight() []string {
	sum := map[string]float64{}
	for _, r := range c.Rules {
		sum[r.Category] += r.Weight
	}
	order := make([]string, 0, len(sum))
	for cat := range sum {
		order = append(order, cat)
	}
	sort.Slice(order, func(i, j int) bool {
		if sum[order[i]] != sum[order[j]] {
			return sum[order[i]] > sum[order[j]]
		}
		return order[i] < order[j]
	})
	return order
}

// Classify scores one message. A nil corpus is treated as empty, so this is
// safe to call per-message without the caller holding the whole store.
func (c *Config) Classify(m android.Message, corpus *Corpus) Result {
	if corpus == nil {
		corpus = NewCorpus(nil)
	}
	lowerBody := strings.ToLower(m.Body)
	lowerSender := strings.ToLower(m.Sender())
	kind := KindOfSender(m.Address)
	length := len([]rune(m.Body))
	bulk := corpus.IsBulk(m.Address, c.BulkTemplate)

	scores := map[string]float64{}
	reasons := map[string][]string{}
	decisive := scored{}

	for _, r := range c.Rules {
		hit, why := r.matches(lowerBody, lowerSender, kind, m, corpus, length, bulk)
		if !hit {
			continue
		}
		scores[r.Category] += r.Weight
		if r.Label != "" {
			reasons[r.Category] = append(reasons[r.Category], r.Label)
		} else {
			reasons[r.Category] = append(reasons[r.Category], r.Category+": "+why)
		}
		if r.Decisive && r.Weight > decisive.score {
			decisive = scored{r.Category, r.Weight}
		}
	}

	res := Result{Category: c.Fallback, Confidence: 0, Reason: "no rule matched"}
	if decisive.score > 0 {
		res.Category = decisive.cat
		res.Score = scores[decisive.cat]
		res.Confidence = 1
		res.Reason = strings.Join(dedupe(reasons[decisive.cat]), ", ")
		return res
	}
	if len(scores) == 0 {
		res.NeedsReview = true
		return res
	}

	ranked := rank(scores)
	top := ranked[0]
	res.Category = top.cat
	res.Score = top.score

	// Strength saturates at Strong: a single weight-1 rule is a hint, two
	// independent ones are a decision.
	strength := math.Min(1, top.score/c.Strong)

	// Margin guards against a coin flip between two weak categories. With
	// only one category in play there is nothing to beat, so it is decisive.
	margin := 1.0
	if len(ranked) > 1 && ranked[1].score > 0 {
		margin = (top.score - ranked[1].score) / top.score
	}
	res.Confidence = clamp(strength * math.Min(1, margin/0.34))
	res.Reason = strings.Join(dedupe(reasons[top.cat]), ", ")
	res.NeedsReview = res.Confidence < c.ReviewBelow
	return res
}

type scored struct {
	cat   string
	score float64
}

// rank orders categories by weight, then by name so ties are deterministic.
func rank(scores map[string]float64) []scored {
	out := make([]scored, 0, len(scores))
	for cat, s := range scores {
		out = append(out, scored{cat, s})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].cat < out[j].cat
	})
	return out
}

// matches reports whether a rule fires, and which of its conditions did. Every
// condition kind present on the rule must hold (AND); within a kind, any entry
// suffices (OR).
func (r Rule) matches(lowerBody, lowerSender string, kind SenderKind, m android.Message, corpus *Corpus, length int, bulk bool) (bool, string) {
	if r.MinLength > 0 && length < r.MinLength {
		return false, ""
	}
	if r.MaxLength > 0 && length > r.MaxLength {
		return false, ""
	}
	if r.ContainsOTP != nil && m.ContainsOTP != *r.ContainsOTP {
		return false, ""
	}
	if r.Inbound != nil && Inbound(m) != *r.Inbound {
		return false, ""
	}
	if r.ThreadReplied != nil && corpus.Replied[m.ThreadID] != *r.ThreadReplied {
		return false, ""
	}
	if r.Bulk != nil && bulk != *r.Bulk {
		return false, ""
	}
	if len(r.SenderKinds) > 0 {
		ok := false
		for _, k := range r.SenderKinds {
			if kind.Name() == k {
				ok = true
				break
			}
		}
		if !ok {
			return false, ""
		}
	}
	if len(r.SenderHas) > 0 {
		ok := false
		for _, s := range r.SenderHas {
			if strings.Contains(lowerSender, strings.ToLower(s)) {
				ok = true
				break
			}
		}
		if !ok {
			return false, ""
		}
	}
	if len(r.SenderIs) > 0 {
		ok := false
		for _, re := range r.SenderIs {
			if re.MatchString(m.Sender()) {
				ok = true
				break
			}
		}
		if !ok {
			return false, ""
		}
	}
	if len(r.BodyHas) > 0 {
		ok := false
		for _, s := range r.BodyHas {
			if strings.Contains(lowerBody, strings.ToLower(s)) {
				ok = true
				break
			}
		}
		if !ok {
			return false, ""
		}
	}
	if len(r.BodyIs) > 0 {
		ok := false
		for _, re := range r.BodyIs {
			if re.MatchString(m.Body) {
				ok = true
				break
			}
		}
		if !ok {
			return false, ""
		}
	}
	// Negative conditions: none of these may be present. They let a broad
	// rule carve out the lookalikes it would otherwise swallow.
	for _, s := range r.NotBodyHas {
		if strings.Contains(lowerBody, strings.ToLower(s)) {
			return false, ""
		}
	}
	for _, re := range r.NotBodyIs {
		if re.MatchString(m.Body) {
			return false, ""
		}
	}
	return true, strings.Join(r.Conditions(), "/")
}

// Conditions lists the condition kinds a rule actually uses — used to build a
// readable reason when the rule has no label.
func (r Rule) Conditions() []string {
	var out []string
	for _, k := range r.SenderKinds {
		out = append(out, k+" sender")
	}
	if len(r.SenderHas) > 0 {
		out = append(out, "sender text")
	}
	if len(r.SenderIs) > 0 {
		out = append(out, "sender regex")
	}
	if len(r.BodyHas) > 0 {
		out = append(out, "body text")
	}
	if len(r.BodyIs) > 0 {
		out = append(out, "body regex")
	}
	if len(r.NotBodyHas) > 0 || len(r.NotBodyIs) > 0 {
		out = append(out, "not body")
	}
	if r.ContainsOTP != nil {
		out = append(out, "otp flag")
	}
	if r.ThreadReplied != nil {
		out = append(out, "thread reply")
	}
	if r.Bulk != nil {
		out = append(out, "bulk sender")
	}
	if r.Inbound != nil {
		out = append(out, "direction")
	}
	return out
}

func clamp(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return math.Round(v*100) / 100
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
		if len(out) == 4 {
			break
		}
	}
	return out
}

// SearchPath lists where a rule file is looked for, most specific first.
func SearchPath() []string {
	if p := os.Getenv("SMS_CLASSIFIER_RULES"); p != "" {
		return []string{p}
	}
	paths := []string{DefaultRulesPath}
	if dir, err := os.UserConfigDir(); err == nil {
		paths = append(paths, filepath.Join(dir, "android-sms-classifier", DefaultRulesPath))
	}
	return paths
}

// WriteTemplate writes the built-in rule template to path.
func WriteTemplate(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists", path)
	}
	return os.WriteFile(path, defaultRules, 0o644)
}

// Describe returns the one-sentence meaning of a category, for the model
// review pass. A category with no description gets a deliberately plain one
// rather than nothing: the model is told what the word means far more reliably
// than it is left to guess.
func (c *Config) Describe(category string) string {
	if d := strings.TrimSpace(c.Descriptions[category]); d != "" {
		return d
	}
	return "Messages that belong in the " + category + " category."
}

// Categories returns the category names in display order, which is the order
// the model is offered them in.
func (c *Config) Categories() []string {
	if len(c.Order) > 0 {
		return c.Order
	}
	return c.categoriesByWeight()
}
