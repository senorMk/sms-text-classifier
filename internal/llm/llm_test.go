package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReview(t *testing.T) {
	var gotModel, gotAuth, gotPrompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		var req struct {
			Model    string                           `json:"model"`
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		gotModel = req.Model
		gotPrompt = req.Messages[0].Content + req.Messages[1].Content
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": "```json\n[{\"id\": 1, \"category\": \"otp\", \"reason\": \"verification code\"}, " +
				"{\"id\": 2, \"category\": \"personal\", \"reason\": \"a friend\"}]\n```"}}},
		})
	}))
	defer srv.Close()

	c, err := New(Config{BaseURL: srv.URL + "/v1", Model: "test-model", APIKey: "sk-x", Batch: 2})
	if err != nil {
		t.Fatal(err)
	}
	var seen []Result
	n, err := c.Review(context.Background(), []Item{
		{ID: 1, Sender: "Google", Body: "code 448201"},
		{ID: 2, Sender: "+260977", Body: "are you coming?"},
	}, []string{"otp", "personal", "banking"}, func(r Result) { seen = append(seen, r) })
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || len(seen) != 2 {
		t.Fatalf("n = %d, seen = %#v", n, seen)
	}
	if seen[0].Category != "otp" || seen[1].Category != "personal" {
		t.Errorf("results = %#v", seen)
	}
	if gotModel != "test-model" {
		t.Errorf("model = %q", gotModel)
	}
	if gotAuth != "Bearer sk-x" {
		t.Errorf("auth = %q", gotAuth)
	}
	for _, want := range []string{"otp, personal, banking", "id=1", "code 448201", "are you coming?"} {
		if !strings.Contains(gotPrompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, gotPrompt)
		}
	}
}

// A hallucinated category or a missing id must not become a verdict.
func TestReviewDropsUnusableResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": `[{"id": 1, "category": "astrology"}, {"id": 0, "category": "otp"}, {"id": 3, "category": "spam"}]`}}},
		})
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, Model: "m"})
	var seen []Result
	n, err := c.Review(context.Background(), []Item{{ID: 1}, {ID: 0}, {ID: 3}}, []string{"otp", "spam"}, func(r Result) { seen = append(seen, r) })
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || len(seen) != 1 || seen[0].ID != 3 {
		t.Errorf("n = %d, seen = %#v, want only id 3", n, seen)
	}
}

func TestReviewEmptyCategoriesIsRejected(t *testing.T) {
	c, _ := New(Config{BaseURL: "http://x", Model: "m"})
	if _, err := c.Review(context.Background(), []Item{{ID: 1}}, []string{"  ", ""}, nil); err == nil {
		t.Error("expected an error with no categories")
	}
}

func TestBatchesRespectBatchSize(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req struct {
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": `[{"id": 1, "category": "otp"}]`}}},
		})
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, Model: "m", Batch: 2})
	items := make([]Item, 5)
	for i := range items {
		items[i] = Item{ID: int64(i + 1)}
	}
	if _, err := c.Review(context.Background(), items, []string{"otp"}, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3 (5 items in batches of 2)", calls)
	}
}

func TestErrorsSurface(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, Model: "m"})
	_, err := c.Review(context.Background(), []Item{{ID: 1}}, []string{"otp"}, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "bad key") {
		t.Errorf("err = %v, want it to carry the status and body", err)
	}
}

func TestContextCancellationStops(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": `[]`}}},
		})
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, Model: "m", Batch: 1})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Review(ctx, []Item{{ID: 1}, {ID: 2}}, []string{"otp"}, nil)
	if err == nil {
		t.Error("expected a cancellation error")
	}
}

func TestValidate(t *testing.T) {
	if _, err := New(Config{Model: ""}); err == nil {
		t.Error("expected an error with no model")
	}
	c, err := New(Config{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if c.cfg.BaseURL != DefaultBaseURL || c.cfg.Batch != 10 || c.cfg.Timeout == 0 {
		t.Errorf("defaults not applied: %#v", c.cfg)
	}
}

func TestParseResults(t *testing.T) {
	cases := map[string]int{
		`[{"id":1,"category":"otp"}]`:                 1,
		"prose\n```json\n[{\"id\":1}]\n```\ntrailing": 1,
		"no json here":        0,
		`[{"id":1},{"id":2}]`: 2,
	}
	for in, want := range cases {
		got, err := parseResults(in)
		if in == "no json here" {
			if err == nil {
				t.Errorf("parseResults(%q) should have errored", in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseResults(%q) = %v", in, err)
			continue
		}
		if len(got) != want {
			t.Errorf("parseResults(%q) = %d results, want %d", in, len(got), want)
		}
	}
}
