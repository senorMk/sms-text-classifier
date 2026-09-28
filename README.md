# Android Text Classifier

Pulls the SMS store off a connected Android device and sorts the messages into
categories — OTPs, banking, delivery, travel, alerts, work, personal, promo,
spam — using an editable rule set, with an optional second-opinion pass for the
residue the rules are unsure about.

An interactive Go CLI: pick a device, browse, live-filter, `esc` to walk back up
its own stages, and flags for when you want a pipe instead of a screen.

```
$ sms-classifier --plain
# 38071FDJG007QW: 14924 messages (12 new)
CATEGORY  MESSAGES  SENDERS  UNSURE  LATEST
otp       1111      74       53      2026-09-28 11:53
banking   5351      38       98      2026-09-27 21:02
delivery  127       9        19      2026-04-23 08:39
travel    2         2        0       2023-04-13 16:38
alerts    3737      41       2       2026-09-27 16:22
work      38        19       1       2026-09-09 08:49
personal  3691      353      54      2026-09-28 12:03
promo     68        20       5       2026-09-19 16:47
spam      19        10       1       2026-04-24 12:49
other     780       121      780     2026-09-28 11:51
```

## Requirements

- `adb` (platform-tools) on `PATH`
- A device with USB debugging enabled

No root and no companion app: messages are read through the SMS content
provider over adb.

## Build

```
go build -o sms-classifier .
```

## Usage

### Interactive (default)

```
./sms-classifier
```

1. **Pick a device** — shown by friendly name (`Google Pixel 8 Pro — 38071FDJG007QW`)
   where the device reports one. Skipped automatically if only one is attached,
   or pass `-s <serial>`. The launch mark sits above the picker and yields to it
   on a short terminal; it is not shown again once you've synced.
2. **Sync** — the first run pulls the whole store; later runs fetch only the
   messages the device gained. Press `s` to re-sync.
3. **Pick a category** — a histogram of what the store actually holds, then
   the categories with their counts, sender counts, and how many verdicts the
   rules were unsure about. Type to filter, `enter` to open, `s` to re-sync, `l`
   to run the review pass, `e` to export.
4. **Browse** — a dense list; type to filter, `enter` to read a message, `e` to
   export what is on screen — including just the rows a filter has left visible.
5. **Read** — full body with the verdict and the reason for it. `←/→` (or `n`/`p`)
   move between messages, `↑/↓` scroll, `e` exports this one message, `esc`
   goes back.

### Non-interactive

```sh
./sms-classifier --plain                      # category summary
./sms-classifier -c otp                       # every OTP message
./sms-classifier -g "flight" --json           # search, as JSON
./sms-classifier --export messages.html      # browsable page
./sms-classifier --export messages.csv       # or plain CSV
./sms-classifier --offline --plain            # from the cache, no adb at all
```

### Flags

| Flag | Description |
| --- | --- |
| `-s`, `--serial` | device serial (skips the device picker) |
| `--refresh` | re-pull the whole store instead of just the tail |
| `--offline` | work from the local cache only; never touch adb |
| `--reclassify` | re-run the rules over the whole store, undoing `--llm` |
| `--since` | only messages newer than this (`720h`, `30d`, …) |
| `--store` | cache directory (default: user config dir) |
| `-c`, `--category` | only this category |
| `-g`, `--search` | only messages matching this text |
| `--plain` | category summary table to stdout |
| `--json` | messages as JSON |
| `--export` | write messages to a `.json`, `.csv` or `.html` file |
| `--rules` | path to `rules.toml` |
| `--init-rules <path>` | write the default `rules.toml` and exit |
| `--paths` | print the resolved cache and rules paths and exit |
| `--review` | run the second-opinion pass on low-confidence messages |
| `--engine` | `typesafe` (default) or `openai` |
| `--review-limit` | max messages to send (default 100) |
| `--typesafe-model` | Jev model id (default `jev-latest`) |
| `--typesafe-key` | API key (default `$TYPESAFE_API_KEY`) |
| `--typesafe-concurrency` | requests in flight (default 8, limit 1200/min) |
| `--llm <model>` | use an OpenAI-compatible model instead |
| `--llm-key` | API key (default `$SMS_CLASSIFIER_LLM_KEY`) |
| `--llm-batch` | messages per OpenAI request (default 10) |

## Exporting

`e` opens a one-line prompt from any of the three browse screens — the category
picker, the message list, and the message reader. Enter with nothing typed and it
writes a timestamped file next to the cache, named after what is on screen:

| Where you are | Exports | Default name |
| --- | --- | --- |
| Category picker | everything | `sms-<serial>-all-<date>.html` |
| Message list | the current category | `sms-<serial>-<category>-<date>.html` |
| Message list, filtered | only the rows you can see | `…-<category>-filtered-<date>.html` |
| Message reader | that one message | `sms-<serial>-message-<id>-<date>.html` |

Type a path to choose your own. The extension picks the format: `.html`, `.csv`
or `.json`. It never overwrites: an existing file is an error, and the path in
the footer tells you where it went. The same three formats are available
as a flag for scripting:

```sh
./sms-classifier --export messages.html
./sms-classifier -c otp --export otps.html
```

Exports are written `0600` and contain your messages.

### The HTML

`--export out.html` writes a single self-contained page — inline CSS and JS, no
external requests, nothing to serve — that you can open straight from disk or
mail yourself. `--category` and `--search` compose with it, so
`-c otp --export otps.html` is just the OTPs.

It gives you the count per category as filter chips, a text filter over
sender/body/reason, a "low confidence only" toggle for the messages the rules
were unsure about, and click-to-expand for long bodies (the text stays in the
page, so the search still reaches it). `/` focuses the filter, `esc` clears it.

Bodies are written through `html/template`, so an SMS arriving from an attacker
renders as the inert text it is — a message containing `<script>` shows up as
`<script>` in the table rather than running. That's the same reason the export
is `0600`: it is your mail.

## The launch mark and the category panel

The mark that opens the session draws three messages wired to the verdicts they
earned — it is what the tool does, rather than a wordmark. Its colours come from
`rules.toml`, so editing your rules changes the launch screen.

The categories screen uses the same frame filled with your real data: a bar per
category, in that category's colour, biggest first, with the tail summarised.
It is live rather than a splash, so it is on every visit to that screen. Both
give way on a narrow or short terminal — the mark falls back to a plain verdict
list below 36 columns and disappears below 18 rows, and the panel is skipped
rather than shown too narrow to read. The device picker always wins the space,
because it is the part you have to use.

## How classification works

Every rule in `rules.toml` contributes weight to one category. The heaviest
category wins, and **the gap to the runner-up is the confidence** — which is
what makes the low-confidence list meaningful rather than a guess:

- a lone weight-1 rule is a hint, two agreeing rules are a decision;
- a 2–2 tie is not a decision, and gets flagged;
- `decisive = true` settles a categorical fact outright, ignoring weight —
  used for the device's own OTP flag, which is a fact and not a hint.

Confidence is stored next to the message, along with the reason for it, so you
can see *why* something landed where it did.

## The cache

Pulled messages are cached as JSON Lines per device under
`~/Library/Application Support/android-sms-classifier/` (mode `0600` — this is
your mail). A sidecar records a hash of the rules, so editing `rules.toml`
re-classifies the whole store on next open instead of showing stale verdicts.
Pulls are incremental: only `_id`s above the highest cached one are fetched.

## Rules

```sh
./sms-classifier --init-rules ./rules.toml   # write a template to edit
```

Searched in order: `--rules` → `./rules.toml` → the user config dir → the
built-in defaults. A rule's conditions are ANDed across kinds and ORed within
one, and `not_*` kinds invert:

```toml
[[rule]]
category = "banking"
label = "card-notification format"
weight = 1.8
body_regex = ['(?i)\b(cheq|cheque)\s*a/c\b', '(?i)reserved for purchase']
```

See the header of the generated file for the full condition list.

## The review pass

Off unless you ask for it. `--review` sends **only** the messages the rules were
unsure about to a second opinion, capped by `--review-limit`, and writes the
verdicts over the top.

Two rules make this safe to point at a model:

- **The rules stay authoritative.** A message the rules were confident about is
  never even offered, so no engine can overturn it. An engine can only supply a
  verdict for something the rules left open.
- **A hesitant verdict goes back in the queue.** Engines that report confidence
  have it stored honestly, so a model that answers at 0.3 is still shown as
  unsure rather than quietly treated as settled.

`--reclassify` re-runs the rules over the whole store and discards every engine
verdict, so the pass is always reversible.

### TypeSafe Jev (default engine)

[Jev](https://docs.typesafe.ai/introduction) is TypeSafe's System One model: you
send a state and typed questions, it returns a typed answer with a probability
per option and a calibrated confidence. That is precisely this job — a
closed-set choice — so there is nothing to parse and no way for it to invent a
category that was not on offer.

```sh
export TYPESAFE_API_KEY=...
./sms-classifier --review                          # the 100 least confident
./sms-classifier --review --review-limit 1000      # more of them
```

It is the default because it is cheaper and better suited than a generative
model: on a 15k-message store the whole store is roughly $0.15 at $0.042/Mtok,
and the ~1,000 low-confidence messages come to about a cent. `--llm` still
implies the pass with an OpenAI-compatible engine, which is what you want for a
local model:

```sh
./sms-classifier --llm llama3 --llm-base-url http://localhost:11434/v1
```

Three things shape `internal/typesafe`:

- **One request per message.** The vendor's guidance is that accuracy falls as
  irrelevant detail is added to the state, so the state is one message plus a
  few facts a rules engine already knows — whether the phone's own retriever
  spotted a passcode, whether the sender is a short code or a person's number.
- **The category descriptions in `rules.toml` are the contract.** This model
  answers the words it is given rather than the words you meant, so the
  `[category.*]` descriptions are sent verbatim as the choice criteria.
- **A message body is data, never instructions.** A received SMS is fully
  attacker-controlled, and this model does not treat state as hostile by
  default. So the body is only ever placed in the state, after a marker, and
  never concatenated into the instructions or the criteria. There is a test
  that asserts exactly this with an injected body.

If you tune `review_below` against confidence values, pin
`--typesafe-model jev-1.13.0` rather than using the `jev-latest` alias, which
moves when a release ships.

## Tests

```
go test ./...
```

The parser tests are built from shapes found on a real device: bodies that
contain newlines, commas, and text that looks exactly like the record separator.

## Layout

```
main.go                     flags, wiring, non-interactive output
internal/android/           adb device discovery, SMS pull, output parser
internal/classify/          rule engine, corpus signals, default rules
internal/store/             JSONL cache, incremental merge, export
internal/llm/               optional OpenAI-compatible review pass
internal/tui/               staged interactive UI
```

## Notes on the SMS provider

`content query` has no escaping and no `LIMIT` support, which shapes two
things in the code: `body` has to be the last projected column (a body
containing `, creator=` would otherwise desync the field split), and pulls page
over `_id` ranges rather than using a row limit. Records are reassembled before
parsing, and a line only starts a record when it carries the row prefix, the
expected index, *and* every column delimiter in order.
