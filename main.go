// This tool pulls the SMS store off a connected Android device and sorts the
// messages into categories, using an editable rule set and an optional LLM pass
// for the residue the rules are unsure about.
//
// Interactive by default (pick device → sync → pick category → browse → read).
// Supply --plain, --json or --export to run non-interactively.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/senorMk/android-text-classifier/internal/android"
	"github.com/senorMk/android-text-classifier/internal/classify"
	"github.com/senorMk/android-text-classifier/internal/llm"
	"github.com/senorMk/android-text-classifier/internal/review"
	"github.com/senorMk/android-text-classifier/internal/store"
	"github.com/senorMk/android-text-classifier/internal/tui"
	"github.com/senorMk/android-text-classifier/internal/typesafe"
)

func main() {
	var (
		serial = flag.String("serial", "", "device serial (skips the device picker)")
		rules  = flag.String("rules", "", "path to rules.toml (default: search the usual places)")
		dir    = flag.String("store", "", "cache directory (default: user config dir)")

		refresh    = flag.Bool("refresh", false, "re-pull the whole store instead of just new messages")
		offline    = flag.Bool("offline", false, "use the local cache only; never touch adb")
		reclassify = flag.Bool("reclassify", false, "re-run the rules over the whole store, discarding LLM verdicts")
		category   = flag.String("category", "", "only this category (non-interactive)")
		search     = flag.String("search", "", "only messages matching this text (non-interactive)")
		since      = flag.Duration("since", 0, "only messages newer than this (e.g. 720h, 30d)")

		plain  = flag.Bool("plain", false, "print a summary table to stdout")
		asJSON = flag.Bool("json", false, "print messages as JSON")
		export = flag.String("export", "", "write messages to a .json or .csv file")

		reviewOn    = flag.Bool("review", false, "run the second-opinion pass on low-confidence messages")
		engineName  = flag.String("engine", "typesafe", "review engine: typesafe or openai")
		tsModel     = flag.String("typesafe-model", typesafe.DefaultModel, "Jev model id (pin a version, not the alias, to keep thresholds stable)")
		tsKey       = flag.String("typesafe-key", os.Getenv("TYPESAFE_API_KEY"), "TypeSafe API key (default $TYPESAFE_API_KEY)")
		tsBase      = flag.String("typesafe-base-url", typesafe.DefaultBaseURL, "TypeSafe endpoint")
		tsConc      = flag.Int("typesafe-concurrency", 8, "TypeSafe requests in flight (rate limit is 1200/min)")
		llmModel    = flag.String("llm", "", "use an OpenAI-compatible model for the pass instead")
		llmBase     = flag.String("llm-base-url", llm.DefaultBaseURL, "OpenAI-compatible base URL")
		llmKey      = flag.String("llm-key", os.Getenv("SMS_CLASSIFIER_LLM_KEY"), "API key (default $SMS_CLASSIFIER_LLM_KEY)")
		reviewLimit = flag.Int("review-limit", 100, "max messages to send for the review pass")
		llmBatch    = flag.Int("llm-batch", 10, "messages per OpenAI request")

		initRules = flag.String("init-rules", "", "write the default rules.toml to this path and exit")
		showPaths = flag.Bool("paths", false, "print the resolved cache and rules paths and exit")
	)
	flag.StringVar(serial, "s", "", "shorthand for --serial")
	flag.StringVar(category, "c", "", "shorthand for --category")
	flag.StringVar(search, "g", "", "shorthand for --search")
	flag.Usage = usage
	flag.Parse()

	if err := run(cfg{
		serial:     *serial,
		rules:      *rules,
		dir:        *dir,
		refresh:    *refresh,
		offline:    *offline,
		reclassify: *reclassify,
		category:   *category,
		search:     *search,
		since:      *since,
		plain:      *plain,
		asJSON:     *asJSON,
		export:     *export,
		review: reviewConfig{
			on: *reviewOn, engine: *engineName, limit: *reviewLimit,
			tsModel: *tsModel, tsKey: *tsKey, tsBase: *tsBase, tsConc: *tsConc,
			llmModel: *llmModel, llmBase: *llmBase, llmKey: *llmKey, llmBatch: *llmBatch,
		},
		initPath: *initRules,
		paths:    *showPaths,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// reviewConfig covers both review engines. Naming a model implies the pass, so
// --llm alone still behaves the way it always did.
type reviewConfig struct {
	on     bool
	engine string
	limit  int

	tsModel, tsKey, tsBase string
	tsConc                 int

	llmModel, llmBase, llmKey string
	llmBatch                  int
}

// enabled reports whether the pass should run at all.
func (r reviewConfig) enabled() bool { return r.on || r.llmModel != "" }

type cfg struct {
	serial, rules, dir string
	refresh            bool
	offline            bool
	reclassify         bool
	category, search   string
	since              time.Duration
	plain, asJSON      bool
	export             string
	review             reviewConfig
	initPath           string
	paths              bool
}

func run(c cfg) error {
	// These two do their own thing and need neither adb nor a device.
	if c.initPath != "" {
		if err := classify.WriteTemplate(c.initPath); err != nil {
			return err
		}
		fmt.Printf("wrote %s\n", c.initPath)
		return nil
	}

	// An explicit --rules must exist: a typo there should not quietly leave
	// you classifying with the built-in defaults.
	var (
		rules *classify.Config
		err   error
	)
	if c.rules != "" {
		rules, err = classify.Load(c.rules)
	} else {
		rules, err = classify.LoadFirst(classify.SearchPath())
	}
	if err != nil {
		return err
	}

	// --offline never shells out to adb at all: it works from the cache, so
	// it is also the fast path for re-reading history.
	var (
		tc    *android.Toolchain
		ready []android.Device
	)
	if !c.offline {
		var err error
		if tc, err = android.Discover(); err != nil {
			return err
		}
		devices, err := tc.Devices()
		if err != nil {
			return err
		}
		ready = readyDevices(devices)
		if len(ready) == 0 {
			return fmt.Errorf("no ready devices (check `adb devices` and USB debugging)")
		}
	}

	if c.paths {
		printPaths(ready, rules)
		return nil
	}

	headless := c.plain || c.asJSON || c.export != "" || c.category != "" || c.search != "" || c.review.enabled()

	// The interactive session picks its own device, so an ambiguous serial is
	// not an error there — it is the first screen.
	if !headless && !c.offline && c.serial == "" && len(ready) > 1 {
		return runInteractive(tc, ready, nil, rules, nil, "", c)
	}

	serial, err := resolveSerial(ready, c.serial, c.dir)
	if err != nil {
		return err
	}

	st, err := store.Open(c.dir, serial)
	if err != nil {
		return err
	}
	// Offline, the TUI is still fine to run: it just has nothing to sync.
	if c.offline {
		// The LLM pass is a network call, not a device one, so it works
		// offline as well.
		engine, err := buildReviewer(c.review, rules)
		if err != nil {
			return err
		}
		if headless {
			return runOffline(st, rules, engine, c)
		}
		return runInteractive(nil, nil, st, rules, engine, serial, c)
	}

	engine, err := buildReviewer(c.review, rules)
	if err != nil {
		return err
	}

	if headless {
		return runHeadless(tc, st, rules, engine, serial, c)
	}

	return runInteractive(tc, ready, st, rules, engine, serial, c)
}

// runOffline serves every output mode straight from the cache, with no adb
// call and no reclassification unless the rules changed.
func runOffline(st *store.Store, rules *classify.Config, engine review.Reviewer, c cfg) error {
	if len(st.Records) == 0 {
		return fmt.Errorf("no cached messages for %s (run once with the device attached)", st.Serial)
	}
	if c.reclassify || st.NeedsClassify(rules) {
		st.Classify(rules)
		if err := st.Save(); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "# %s: %d cached messages, rules %s (offline)\n",
		st.Serial, len(st.Records), rules.Fingerprint())

	if engine != nil {
		n, err := runReview(st, rules, engine, c.review)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "# %s: judged %d low-confidence messages with %s\n",
			engine.Name(), n, engine.Model())
		if err := st.Save(); err != nil {
			return err
		}
	}

	records := selectRecords(st, c)
	switch {
	case c.asJSON:
		return store.ExportJSON(os.Stdout, records)
	case c.export != "":
		return export(records, c.export, rules, st.Serial)
	case c.category != "" || c.search != "":
		return printMessages(records)
	}
	return printSummary(st, rules)
}

// runHeadless syncs, classifies, optionally runs the LLM pass, and writes the
// requested output.
func runHeadless(tc *android.Toolchain, st *store.Store, rules *classify.Config, engine review.Reviewer, serial string, c cfg) error {
	pulled, err := sync(tc, st, rules, serial, c)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "# %s: %d messages (%d new)\n", serial, len(st.Records), pulled)

	records := selectRecords(st, c)

	if engine != nil {
		n, err := runReview(st, rules, engine, c.review)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "# %s: judged %d low-confidence messages with %s\n",
			engine.Name(), n, engine.Model())
		// The model's verdicts may have moved records between categories.
		records = selectRecords(st, c)
		if err := st.Save(); err != nil {
			return err
		}
	} else if err := st.Save(); err != nil {
		return err
	}

	switch {
	case c.asJSON:
		return store.ExportJSON(os.Stdout, records)
	case c.export != "":
		return export(records, c.export, rules, serial)
	case c.category != "" || c.search != "":
		return printMessages(records)
	}
	return printSummary(st, rules)
}

// sync pulls new messages and re-classifies when the stored verdicts are
// missing or stale. It returns how many messages were added.
func sync(tc *android.Toolchain, st *store.Store, rules *classify.Config, serial string, c cfg) (int, error) {
	if c.refresh {
		st.Records = nil
	}
	opt := android.PullOptions{AfterID: st.MaxID()}
	if c.since > 0 {
		opt.Since = time.Now().Add(-c.since)
	}
	msgs, err := tc.Messages(serial, opt)
	if err != nil {
		return 0, err
	}
	added := st.Merge(msgs)
	// A fresh store has no verdicts, a partial pull leaves new rows
	// unclassified, an edited rules file invalidates the old ones, and
	// --reclassify discards whatever the LLM pass decided.
	if c.reclassify || st.NeedsClassify(rules) {
		st.Classify(rules)
	}
	return added, nil
}

// runReview performs the pass against a store, reporting how many verdicts
// landed. The store is saved by the caller.
func runReview(st *store.Store, rules *classify.Config, engine review.Reviewer, c reviewConfig) (int, error) {
	return review.Run(context.Background(), st, rules, engine, review.Options{
		Limit: c.limit,
		Below: rules.ReviewBelow,
	})
}

func runInteractive(tc *android.Toolchain, devices []android.Device, st *store.Store, rules *classify.Config, engine review.Reviewer, serial string, c cfg) error {
	// The store is opened in the TUI's own sync stage so a device change
	// mid-session re-reads the right cache; the one opened here (when there
	// is one) is only used to report a warm cache up front.
	if st != nil && len(st.Records) > 0 && !st.Stale(rules) {
		fmt.Fprintf(os.Stderr, "# %s: %d cached messages, press s in the UI to re-sync\n", serial, len(st.Records))
	}

	opt := tui.Options{
		StoreDir:    c.dir,
		Rules:       rules,
		Refresh:     c.refresh,
		Offline:     c.offline,
		Reviewer:    engine,
		ReviewLimit: c.review.limit,
	}
	switch {
	case c.offline:
		// No device to pick from; the serial names which cache to open.
		opt.Serial = serial
	case c.serial != "":
		opt.Serial = c.serial
	}
	// With more than one device attached the TUI shows its own picker.
	m := tui.New(tc, devices, opt)
	p := tea.NewProgram(&m, tea.WithAltScreen())
	final, err := p.Run()
	if err != nil {
		return err
	}
	if m, ok := final.(*tui.Model); ok {
		return m.Err()
	}
	return nil
}

// --- output ---

// printSummary is the default headless view: one row per category, newest
// first, so a glance says what the inbox is made of.
func printSummary(st *store.Store, rules *classify.Config) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "CATEGORY\tMESSAGES\tSENDERS\tUNSURE\tLATEST")
	for _, b := range st.Buckets(rules) {
		latest := "-"
		for _, r := range st.In(b.Category) {
			latest = r.Date.Local().Format("2006-01-02 15:04")
			break
		}
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%s\n", b.Category, b.Count, b.Senders, b.LowConf, latest)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "\n# total %d messages · store %s\n", len(st.Records), st.Path)
	return nil
}

// printMessages lists individual messages for a --category/--search run.
func printMessages(records []store.Record) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "WHEN\tCATEGORY\tFROM\tBODY")
	for _, r := range records {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			r.Date.Local().Format("2006-01-02 15:04"),
			r.Class.Category,
			truncateCell(r.Sender(), 24),
			truncateCell(oneLine(r.Body), 90),
		)
	}
	return w.Flush()
}

func export(records []store.Record, path string, rules *classify.Config, serial string) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	// Message bodies are private; keep the file owner-only like the cache.
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		err = store.ExportJSON(f, records)
	case ".csv":
		err = store.ExportCSV(f, records)
	case ".html", ".htm":
		w := bufio.NewWriter(f)
		err = store.ExportHTML(w, records, store.HTMLOptions{
			Serial:      serial,
			Title:       htmlTitle(records, rules),
			Rules:       rules.Source,
			Order:       rules.Order,
			Colors:      rules.Colors,
			ReviewBelow: rules.ReviewBelow,
		})
		if err == nil {
			err = w.Flush()
		}
	default:
		return fmt.Errorf("export: use a .json, .csv or .html extension, got %q", filepath.Ext(path))
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "# wrote %d messages to %s\n", len(records), path)
	return nil
}

// htmlTitle names the page after whatever narrowed the export, so a filtered
// file says what is in it when it lands in a downloads folder.
func htmlTitle(records []store.Record, rules *classify.Config) string {
	if len(records) == 0 {
		return "SMS messages"
	}
	cats := map[string]bool{}
	for _, r := range records {
		cats[r.Class.Category] = true
	}
	if len(cats) == 1 {
		for c := range cats {
			return "SMS — " + c
		}
	}
	return fmt.Sprintf("SMS messages — %s (%s)", records[0].Sender(), rules.Fingerprint())
}

// selectRecords applies the --category / --search filters.
func selectRecords(st *store.Store, c cfg) []store.Record {
	switch {
	case c.category != "":
		return st.In(c.category)
	case c.search != "":
		return searchRecords(st.Records, c.search)
	}
	return st.Records
}

func searchRecords(records []store.Record, q string) []store.Record {
	needle := strings.ToLower(q)
	var out []store.Record
	for _, r := range records {
		if strings.Contains(strings.ToLower(r.Body), needle) ||
			strings.Contains(strings.ToLower(r.Sender()), needle) ||
			strings.Contains(strings.ToLower(r.Class.Category), needle) {
			out = append(out, r)
		}
	}
	return out
}

func printPaths(ready []android.Device, rules *classify.Config) {
	if len(ready) > 0 {
		fmt.Println("# adb devices")
		for _, d := range ready {
			fmt.Printf("#   %s\n", d.Label())
		}
	} else if cached, err := store.ListSerials(""); err == nil && len(cached) > 0 {
		fmt.Println("# cached devices (no device attached)")
		for _, s := range cached {
			fmt.Printf("#   %s\n", s)
		}
	}
	fmt.Println("# rules in force:", rules.Source, "("+rules.Fingerprint()+")")
	fmt.Println("# rules searched:", strings.Join(classify.SearchPath(), ", "))
	dir, err := os.UserConfigDir()
	if err != nil {
		return
	}
	fmt.Println("# default store dir:", filepath.Join(dir, "android-sms-classifier"))
}

// --- helpers ---

// buildReviewer constructs the chosen engine, or nil when the pass is off.
//
// The default is TypeSafe's Jev because it answers a closed-set question with a
// calibrated confidence and nothing to parse, which is exactly this job. The
// OpenAI-compatible path stays because it reaches local models.
func buildReviewer(c reviewConfig, rules *classify.Config) (review.Reviewer, error) {
	if !c.enabled() {
		return nil, nil
	}
	// Naming a model picks the engine for you, so --llm keeps working alone.
	engine := c.engine
	if c.llmModel != "" {
		engine = "openai"
	}
	switch engine {
	case "typesafe", "jev":
		return typesafe.New(typesafe.Config{
			BaseURL:     c.tsBase,
			Model:       c.tsModel,
			APIKey:      c.tsKey,
			Concurrency: c.tsConc,
			Describe:    rules.Describe,
		})
	case "openai":
		if c.llmModel == "" {
			return nil, fmt.Errorf("--engine openai needs a model: pass --llm <model>")
		}
		if c.llmKey == "" {
			// A local model (Ollama, LM Studio, vLLM) needs no key, so only
			// insist when the endpoint looks like a hosted one.
			if strings.Contains(c.llmBase, "api.openai.com") {
				return nil, fmt.Errorf("--llm needs a key: pass --llm-key or set $SMS_CLASSIFIER_LLM_KEY")
			}
			fmt.Fprintln(os.Stderr, "# no --llm-key given; assuming a local endpoint")
		}
		return llm.New(llm.Config{
			BaseURL: c.llmBase,
			Model:   c.llmModel,
			APIKey:  c.llmKey,
			Batch:   c.llmBatch,
		})
	}
	return nil, fmt.Errorf("unknown --engine %q, want typesafe or openai", engine)
}

func readyDevices(devices []android.Device) []android.Device {
	var ready []android.Device
	for _, d := range devices {
		if d.Ready() {
			ready = append(ready, d)
		}
	}
	return ready
}

// resolveSerial picks the device, defaulting to the only one attached. With no
// devices (offline) it falls back to the only cache that exists.
func resolveSerial(ready []android.Device, serial, dir string) (string, error) {
	if serial == "" {
		switch len(ready) {
		case 0:
			cached, err := store.ListSerials(dir)
			if err != nil {
				return "", err
			}
			switch len(cached) {
			case 0:
				return "", fmt.Errorf("no cached messages found; run once with a device attached")
			case 1:
				return cached[0], nil
			}
			return "", fmt.Errorf("several caches here (%s); pass --serial", strings.Join(cached, ", "))
		case 1:
			return ready[0].Serial, nil
		}
		var names []string
		for _, d := range ready {
			names = append(names, d.Serial)
		}
		sort.Strings(names)
		return "", fmt.Errorf("multiple devices attached (%s); pass --serial", strings.Join(names, ", "))
	}

	if len(ready) > 0 {
		for _, d := range ready {
			if d.Serial == serial {
				return serial, nil
			}
		}
		return "", fmt.Errorf("device %q is not attached and ready", serial)
	}
	// Offline: an explicit serial is taken at face value, but there had better
	// be a cache under it.
	cached, err := store.ListSerials(dir)
	if err != nil {
		return "", err
	}
	if len(cached) == 0 {
		return serial, nil
	}
	for _, s := range cached {
		if s == serial {
			return serial, nil
		}
	}
	return "", fmt.Errorf("no cache for device %q (have: %s)", serial, strings.Join(cached, ", "))
}

func truncateCell(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func usage() {
	fmt.Fprint(os.Stderr, `sms-classifier — pull an Android SMS store and sort it into categories

Usage:
  sms-classifier                          # fully interactive (press e to export)
  sms-classifier --plain                  # category summary to stdout
  sms-classifier -c otp                  # every OTP message
  sms-classifier -g "flight" --json       # search, as JSON
  sms-classifier --export messages.html   # browsable page, filterable offline
  sms-classifier --export messages.csv    # or plain CSV

Device:
  -s, --serial    device serial (skips the device picker)
      --refresh    re-pull the whole store instead of just new messages
      --offline    work from the local cache only; never touch adb
      --reclassify re-run the rules over the whole store, undoing --llm
      --since     only messages newer than this (720h, 30d, …)
      --store     cache directory (default: user config dir)
      --paths     print resolved cache and rules paths

Selecting:
  -c, --category  only this category (implies non-interactive)
  -g, --search    only messages matching this text (implies non-interactive)

Output (any of these implies non-interactive; e in the TUI does the same):
      --plain     category summary table to stdout
      --json      messages as JSON
      --export    write messages to a .json, .csv or .html file

Rules:
      --rules     path to rules.toml (default: ./rules.toml, then the
                  user config dir, then the built-in defaults)
      --init-rules <path>   write the default rules.toml and exit

Review pass (off unless asked for; only messages the rules were unsure about
are ever sent, and a confident rule verdict is never overridden):
      --review        run the pass with the default engine
      --engine        typesafe (default) or openai
      --review-limit  max messages to send (default 100)

  TypeSafe Jev — built for closed-set decisions, returns a calibrated
  confidence with nothing to parse:
      --typesafe-model       model id, default jev-latest (pin a version to
                             keep confidence thresholds stable)
      --typesafe-key         default $TYPESAFE_API_KEY
      --typesafe-base-url    default https://api.typesafe.ai
      --typesafe-concurrency requests in flight, default 8 (limit 1200/min)

  Any OpenAI-compatible endpoint, for local models:
      --llm <model>          implies --review --engine openai
      --llm-base-url        default https://api.openai.com/v1
      --llm-key             default $SMS_CLASSIFIER_LLM_KEY
      --llm-batch           messages per request, default 10

Requires adb on PATH and a device with USB debugging enabled. Messages are
read through the SMS content provider, so no root is needed — and nothing
leaves the machine unless you ask for the review pass.
`)
}
