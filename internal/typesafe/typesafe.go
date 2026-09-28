// Package typesafe is the default review engine: Jev, TypeSafe's System One
// model, reached over its plain HTTP API.
//
// Why this engine and not a generative one: the job is a closed-set choice with
// a calibrated confidence, and Jev is built for exactly that. It returns a
// typed answer, a probability for every option, and a confidence, with nothing
// to parse and no way to invent a category that was not on offer.
//
// Two constraints from the vendor's own documentation shape this file:
//
//   - Accuracy falls as the state grows with irrelevant detail, so state is
//     one message plus a few facts and nothing else.
//   - The model answers the words it is given rather than the words meant, so
//     the category descriptions in rules.toml are the contract, and they are
//     sent verbatim.
package typesafe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/senorMk/android-text-classifier/internal/review"
)

// DefaultBaseURL is the System One endpoint root; /v1/systemone is appended.
const DefaultBaseURL = "https://api.typesafe.ai"

// DefaultModel is the alias for the current stable release. Callers that tune
// thresholds against a confidence should pin a versioned id instead, since an
// alias moves when a release ships.
const DefaultModel = "jev-latest"

// Config describes the endpoint.
type Config struct {
	BaseURL string
	Model   string
	APIKey  string
	// Concurrency is how many requests are in flight at once. The rate limit
	// is 1,200 requests per minute, so this is the throughput lever: each
	// request judges one message.
	Concurrency int
	Timeout     time.Duration
	// Describe supplies the one-sentence meaning of each category. Without
	// it the model is left to guess what the names mean, which it will not
	// do well.
	Describe func(category string) string
}

// Validate fills in defaults and rejects a config that cannot work.
func (c *Config) Validate() error {
	if c.APIKey == "" {
		return fmt.Errorf("no API key: pass --typesafe-key or set $TYPESAFE_API_KEY")
	}
	if c.Model == "" {
		c.Model = DefaultModel
	}
	if c.BaseURL == "" {
		c.BaseURL = DefaultBaseURL
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 8
	}
	if c.Timeout <= 0 {
		c.Timeout = 60 * time.Second
	}
	return nil
}

// Client is the engine.
type Client struct {
	cfg Config
	hc  *http.Client

	// inFlight throttles concurrent requests, and stopped lets a cancelled
	// context stop the ones still queued.
	slots  chan struct{}
	mu     sync.Mutex
	closed bool
}

// New validates the config and builds a client.
func New(cfg Config) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Client{
		cfg:   cfg,
		hc:    &http.Client{Timeout: cfg.Timeout},
		slots: make(chan struct{}, cfg.Concurrency),
	}, nil
}

// Name satisfies review.Reviewer.
func (c *Client) Name() string { return "jev" }

// Model satisfies review.Reviewer.
func (c *Client) Model() string { return c.cfg.Model }

// question is the one question asked per message. Kept plain: this model reads
// scoping words and implied conditions at face value, so there is nothing to be
// clever about here.
const question = "Which single category best describes this SMS message?"

// tieBreak asks for one answer when two categories both look plausible, which
// is the only place a tie is likely and the only judgement it should make.
const tieBreak = "Choose the category that describes the message's main purpose. Choose exactly one."

// Review judges each item in its own request, one message per call, because
// accuracy falls as unrelated material is added to the state. Calls run
// concurrently up to the configured limit and verdicts stream back as they land.
func (c *Client) Review(ctx context.Context, items []review.Item, categories []string, onResult func(review.Result)) (int, error) {
	if len(items) == 0 {
		return 0, nil
	}
	if len(categories) == 0 {
		return 0, fmt.Errorf("no categories to choose from")
	}
	criteria := map[string]string{}
	for _, cat := range categories {
		desc := cat
		if c.cfg.Describe != nil {
			desc = c.cfg.Describe(cat)
		}
		criteria[cat] = desc
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		count   int
		firstEr error
	)
	emit := func(r review.Result) {
		mu.Lock()
		count++
		mu.Unlock()
		if onResult != nil {
			onResult(r)
		}
	}
	fail := func(err error) {
		mu.Lock()
		if firstEr == nil {
			firstEr = err
		}
		mu.Unlock()
	}

	for _, it := range items {
		if err := ctx.Err(); err != nil {
			break
		}
		if c.isClosed() {
			break
		}
		select {
		case c.slots <- struct{}{}:
		case <-ctx.Done():
		}
		if c.isClosed() {
			break
		}

		wg.Add(1)
		go func(it review.Item) {
			defer wg.Done()
			defer func() { <-c.slots }()

			res, err := c.judge(ctx, it, criteria)
			if err != nil {
				fail(err)
				return
			}
			if res.Category != "" {
				emit(res)
			}
		}(it)
	}
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return count, err
	}
	return count, firstEr
}

// isClosed reports whether Close has been called.
func (c *Client) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// Close stops accepting work. In-flight requests finish; queued ones do not
// start, which is what makes ctrl-c feel immediate on a large store.
func (c *Client) Close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
}

// --- one request ---

// request is the System One payload.
type request struct {
	State     string         `json:"state"`
	Model     string         `json:"model"`
	Questions map[string]any `json:"questions"`
}

type response struct {
	Model   string `json:"model"`
	Answers struct {
		Category struct {
			Type          string             `json:"type"`
			Choice        string             `json:"choice"`
			Confidence    float64            `json:"confidence"`
			Probabilities map[string]float64 `json:"probabilities"`
		} `json:"category"`
	} `json:"answers"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// judge asks for one message's category.
func (c *Client) judge(ctx context.Context, it review.Item, criteria map[string]string) (review.Result, error) {
	body, err := json.Marshal(request{
		State: buildState(it),
		Model: c.cfg.Model,
		Questions: map[string]any{
			"category": map[string]any{
				"type":         "choice",
				"instructions": question + " " + tieBreak,
				"criteria":     criteria,
			},
		},
	})
	if err != nil {
		return review.Result{}, err
	}

	endpoint := strings.TrimRight(c.cfg.BaseURL, "/") + "/v1/systemone"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return review.Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	resp, err := c.hc.Do(req)
	if err != nil {
		return review.Result{}, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return review.Result{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return review.Result{}, fmt.Errorf("typesafe %s: %s", resp.Status, snippet(raw))
	}
	var out response
	if err := json.Unmarshal(raw, &out); err != nil {
		return review.Result{}, fmt.Errorf("unreadable response: %w", err)
	}
	if out.Answers.Category.Choice == "" {
		return review.Result{}, fmt.Errorf("no answer for message %d", it.ID)
	}
	return review.Result{
		ID:         it.ID,
		Category:   out.Answers.Category.Choice,
		Reason:     describeVerdict(out.Answers.Category.Probabilities, out.Answers.Category.Choice),
		Confidence: out.Answers.Category.Confidence,
	}, nil
}

// buildState is the whole world the model sees for one message: who it is
// from, what the phone already knows, and the text. Nothing else, because
// everything else is noise that costs accuracy.
func buildState(it review.Item) string {
	var b strings.Builder
	b.WriteString("SMS message\n")
	if sender := strings.TrimSpace(it.Sender); sender != "" {
		fmt.Fprintf(&b, "from: %s\n", truncate(sender, 120))
	}
	for _, f := range it.Facts {
		b.WriteString(f + "\n")
	}
	b.WriteString("body:\n")
	b.WriteString(truncate(strings.TrimSpace(it.Body), review.MaxBodyChars))
	return b.String()
}

// describeVerdict turns the probability spread into a short reason, so the UI
// and the HTML export can show why — the same way the rules do.
//
// The winner's share is looked up by name rather than taken as the largest,
// so the reason can never contradict the choice that was returned.
func describeVerdict(probs map[string]float64, choice string) string {
	if len(probs) == 0 {
		return ""
	}
	pct := func(p float64) int { return int(p*100 + 0.5) }

	var b strings.Builder
	fmt.Fprintf(&b, "%s %d%%", choice, pct(probs[choice]))

	// Name the strongest alternative, and only if it means something:
	// "otp 60%, then everything else 0%" is noise.
	runner, runnerP := "", 0.0
	for cat, p := range probs {
		if cat == choice || p <= runnerP {
			continue
		}
		runner, runnerP = cat, p
	}
	if runner != "" && runnerP > 0.05 {
		fmt.Fprintf(&b, ", then %s %d%%", runner, pct(runnerP))
	}
	return b.String()
}

func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	// Cut on a rune boundary and say it was cut, so the model isn't handed a
	// half character.
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
