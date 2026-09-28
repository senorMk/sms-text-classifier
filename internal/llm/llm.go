// Package llm is the fallback review engine: any OpenAI-compatible
// /chat/completions endpoint. It is kept because it reaches local models
// (Ollama, LM Studio, vLLM) and every hosted provider, where the default engine
// may not be wanted.
//
// It is deliberately the weakest link in the chain, not the strongest: the
// deterministic rules decide everything first, and only the low-confidence
// residue is offered to a model — and only when the user explicitly asks for
// it. Message bodies leave the machine only then, and the count is capped.
//
// It is also the weaker engine: a generative model has to be coerced into
// emitting JSON and the output parsed, and it reports no calibrated
// confidence. For that reason it is not the default.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/senorMk/android-text-classifier/internal/review"
)

// Config describes the OpenAI-compatible endpoint to talk to. Any provider that
// serves /chat/completions works: OpenAI, Groq, OpenRouter, Together, vLLM,
// Ollama, LM Studio.
type Config struct {
	BaseURL string
	Model   string
	APIKey  string
	Batch   int
	Timeout time.Duration
}

// DefaultBaseURL is OpenAI's API root; the client appends /chat/completions.
const DefaultBaseURL = "https://api.openai.com/v1"

// Validate checks the config is usable before any device time is spent.
func (c *Config) Validate() error {
	if c.Model == "" {
		return fmt.Errorf("no model given (--llm-model, or use --llm <model>)")
	}
	if c.BaseURL == "" {
		c.BaseURL = DefaultBaseURL
	}
	if c.Batch <= 0 {
		c.Batch = 10
	}
	if c.Timeout <= 0 {
		c.Timeout = 60 * time.Second
	}
	return nil
}

// Item is a message to be judged. The same shape every engine takes.
type Item = review.Item

// Result is the model's verdict for one item. Confidence stays 0: a generative
// model reports no calibrated certainty, and inventing one would be a lie the
// confidence gating would then act on.
type Result = review.Result

// Client calls the endpoint.
type Client struct {
	cfg Config
	hc  *http.Client
}

// New validates the config and builds a client.
func New(cfg Config) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Client{cfg: cfg, hc: &http.Client{Timeout: cfg.Timeout}}, nil
}

// Name satisfies review.Reviewer.
func (c *Client) Name() string { return "openai" }

// Model reports the model in use, for display.
func (c *Client) Model() string { return c.cfg.Model }

// Review satisfies review.Reviewer. It sends items in batches and calls
// onResult for each verdict as it lands, so a long run shows progress instead
// of going quiet. It stops early if ctx is cancelled.
func (c *Client) Review(ctx context.Context, items []Item, categories []string, onResult func(Result)) (int, error) {
	valid := cleanCategories(categories)
	if len(valid) == 0 {
		return 0, fmt.Errorf("no categories to choose from")
	}
	set := make(map[string]bool, len(valid))
	for _, c := range valid {
		set[c] = true
	}

	done := 0
	for start := 0; start < len(items); start += c.cfg.Batch {
		if err := ctx.Err(); err != nil {
			return done, err
		}
		end := min(start+c.cfg.Batch, len(items))

		results, err := c.batch(ctx, items[start:end], valid)
		if err != nil {
			return done, fmt.Errorf("batch %d-%d: %w", start+1, end, err)
		}
		for _, r := range results {
			// A model that invents a category we don't know about would
			// create a bucket nothing else can match; drop those.
			if !set[r.Category] || r.ID == 0 {
				continue
			}
			done++
			if onResult != nil {
				onResult(r)
			}
		}
	}
	return done, nil
}

// batch is a single request/response round trip.
func (c *Client) batch(ctx context.Context, items []Item, categories []string) ([]Result, error) {
	body, err := json.Marshal(map[string]any{
		"model":       c.cfg.Model,
		"temperature": 0,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt(categories)},
			{"role": "user", "content": userPrompt(items)},
		},
	})
	if err != nil {
		return nil, err
	}

	endpoint := strings.TrimRight(c.cfg.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", resp.Status, snippet(raw))
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("unreadable response: %w", err)
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("no choices in response")
	}
	return parseResults(out.Choices[0].Message.Content)
}

func systemPrompt(categories []string) string {
	return strings.Join([]string{
		"You classify SMS messages into exactly one category.",
		"Allowed categories: " + strings.Join(categories, ", ") + ".",
		"",
		"Rules:",
		"- Use only the allowed category names, verbatim.",
		"- A one-time passcode or verification code is always 'otp', whatever else the message says.",
		"- Money, cards, transfers and balances are 'banking'.",
		"- Bulk advertising is 'promo'. Fraud, phishing and prize scams are 'spam'.",
		"- A message from a person having a conversation is 'personal'.",
		"- Judge the message, not the sender id alone.",
		"- Keep each reason under 12 words.",
		"",
		"Reply with a JSON array only, no prose and no code fences:",
		`[{"id": 123, "category": "otp", "reason": "..."}]`,
	}, "\n")
}

func userPrompt(items []Item) string {
	var b strings.Builder
	for _, it := range items {
		fmt.Fprintf(&b, "id=%d | from=%s | %s\n", it.ID, it.Sender, it.Body)
	}
	return b.String()
}

// parseResults pulls the JSON array out of a reply, tolerating the code fences
// and preamble models sometimes wrap it in.
func parseResults(content string) ([]Result, error) {
	start := strings.Index(content, "[")
	end := strings.LastIndex(content, "]")
	if start < 0 || end < start {
		return nil, fmt.Errorf("no JSON array in reply: %s", snippet([]byte(content)))
	}
	var out []Result
	if err := json.Unmarshal([]byte(content[start:end+1]), &out); err != nil {
		return nil, fmt.Errorf("unparseable JSON array: %w", err)
	}
	return out, nil
}

// cleanCategories trims and de-duplicates the allowed set.
func cleanCategories(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range in {
		if c = strings.TrimSpace(c); c != "" && !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
