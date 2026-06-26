# Desktop capture architecture

How the desktop client (`desktop/`) collects local AI-tool usage. Covers the *what*
and the *why* — the rationale is not obvious from the code alone.

## Where this fits

The client is **thin**: `capture → redact → upload`. Parsing, normalization, analysis,
and reporting are **all server-side**. The client stays small. **Do not add analysis to the client.**

This doc covers **capture** and **redact**. **Upload** (and the device-code auth it rides on)
is implemented — `aiscan run` does capture → redact → upload, and `aiscan login` authorizes
ahead of time; see [auth and upload](#auth-and-upload) below.

## The seam: one common Artifact type

Capture is the *only* part that is per-source (per LLM). Redact and upload are shared and run once
over everything. The whole design hinges on every source producing the same type:

```go
type Artifact struct {
    Source SourceID // "claude-code" — server picks the parser; redact/upload ignore it
    Path   string   // logical path within the upload dump, slash-separated
    Data   []byte   // raw bytes, NOT normalized
}
```

Because redact and upload depend only on `Artifact`, they never know which source produced
the data. Adding a source changes nothing shared.

## The recipe pattern

Sources are expressed as **data** — a flat slice of `Recipe`, each a detect/capture pair:

```go
type Recipe struct {
    ID      SourceID
    Detect  func() bool                                   // is the tool installed?
    Capture func(ctx, opts) ([]Artifact, error)           // read its raw files
}
```

`capture.Run` iterates the enabled recipes, skips sources whose `Detect` is false, and
concatenates their artifacts. A failing source contributes an error but does not abort the
others — one broken source must not block the rest of the capture.

**Why recipes (data) instead of a config file:** local capture does not share a
parameterizable shape. Claude Code is "read `~/.claude/projects/*.jsonl`"; Cursor is "open
a SQLite `state.vscdb` and decode version-specific keys"; each tool is different. A
server-fetched JSON "recipe language" would just be Go with extra steps. (The *browser
extension* is the opposite case — every web source is "hit these URLs, pluck these JSON
fields" — so it *is* config-driven. Same idea, different medium.) Keeping desktop recipes
as in-code structs gives the "add one list entry per source" ergonomics without inventing a
config language.

## Layout

```
desktop/internal/
  capture/capture.go        Recipe, Artifact, SourceID, Run()   — the seam
  capture/claude/claude.go  Claude Code source                  — per-source logic
  redact/redact.go          shared, source-agnostic secret stripping
  upload/upload.go          shared, source-agnostic (gzip tar → POST /api/aiscan/ingest)
  auth/auth.go              device-code OAuth + on-disk token cache
  cli/capture.go            `aiscan capture` verb + the recipe list
```

The recipe list lives in `cli`, which imports both `capture` and each source package. This
keeps the dependency direction clean (`cli → {capture, source}`, `source → capture`) with
no import cycle and no registration magic.

## Raw, not normalized — on purpose

Capture uploads a **raw source dump**. It does **not** parse or normalize client-side, even
though that would shrink the payload. Two reasons:

- **Decoupling.** Normalizing on the client re-couples it to the parser — exactly the
  brittle, hard-to-update code we moved server-side. The server owns *versioned* parsers so
  it can handle multiple source-format versions as vendors change their formats.
- **Size is a non-issue.** JSONL compresses ~10–20× with gzip; even heavy users are a few MB.

The dump mirrors the on-disk layout under a per-source prefix
(`claude-code/projects/<proj>/<session>.jsonl`) so it is self-describing.

## Sources

### Claude Code (`capture/claude`)
Reads `~/.claude/projects/**/*.jsonl`. `--window-days N` drops files modified before the
cutoff. Non-`.jsonl` files are ignored. Raw bytes only — no parsing.

### Adding a source
1. Write `capture/<tool>/<tool>.go` exposing a `capture.Recipe`.
2. Add one line to the `recipes` list in `cli/capture.go`.

Nothing in `redact` or `upload` changes. (Cursor will be the first source added this way;
its data is local SQLite, which is why it belongs here and not in the browser extension.)

## Redaction

`redact.Redact([]Artifact)` runs once over every source's bytes before anything is
shown or written — it is the only gate before the wire, so `aiscan capture` applies it
by default (`--no-redact` skips it for debugging). It is conservative and targets things
that match a **reliable pattern**:

- **Secrets:** PEM private keys, `sk-`/GitHub/AWS/Slack/Google key shapes, `Bearer`
  tokens, and secret-named assignments (`"API_KEY": "..."`, `DB_PASSWORD=...`).
- **Emails:** all addresses (PII).

Matches are swapped for `[REDACTED]` *inside* JSON string values, so redacted JSONL stays
well-formed. Rules are intentionally stable (`RulesetVersion`); a churny redactor would
reintroduce the fat-client problem.

`aiscan capture` prints a one-line redaction summary (the trust surface): how many hits each
rule made, or `nothing matched`. `--show-redactions` additionally lists every match with the
artifact (project/session) it came from — note this prints the *matched secret values* to the
terminal, so it is a debug aid, not for routine use.

**Personal names are deliberately *not* redacted here.** They have no reliable pattern: a
dictionary of names false-positives on ordinary words (Mark, Rose, Bill, Grace), and NER
needs an ML model — the antithesis of a thin, stable, offline redactor. Reliable name
handling belongs server-side (which has the budget for it) or in the user-controlled
exclusion below. A cheap, deterministic exception worth doing is redacting the *known
local identity* (git `user.name`/`user.email`, OS user) — that catches the operator's own
name with zero false positives, without trying to detect arbitrary names.

## Auth and upload

After redaction, `aiscan run` ships the dump to the server. Two shared, source-agnostic
packages handle it:

- **`auth`** — device-code OAuth (RFC 8628) against the configured instance, matching the
  browser extension's flow so both clients use the same endpoints
  (`/api/oauth/device-authorization/` → `/api/oauth/token/`). The client is the public,
  well-known `sleuth-aiscan` client and holds **no embedded secret**; the only credential it
  stores is the short-lived access token, cached at `<config-dir>/aiscan/token.json` (mode
  `0600`, overridable with `AISCAN_CONFIG_DIR`) until ~60s before it expires. `aiscan login`
  front-loads the approval; `aiscan run` also authorizes on first use.
- **`upload`** — gzips a tar of the redacted artifacts and POSTs it to
  `{instance}/api/aiscan/ingest?source=<id>&window_days=N` with a `Bearer` token, mirroring the
  extension's proven wire format. Each artifact's leading source-id segment is stripped so the
  archive mirrors the tool's native layout (`claude-code/projects/p/s.jsonl` → `projects/p/s.jsonl`,
  i.e. `~/.claude/projects`). A `401` clears the cached token and re-authorizes once. The server
  returns a run id, which the client renders as a report link.
  - **Size limit / batching.** The server streams the ingest body, so the cap is the app's
    `MAX_UPLOAD_BYTES` (50 MiB on the *compressed* body). The client packs under `MaxCompressedBytes`
    (45 MiB, measured) so a typical history uploads as a **single** batch — one scan session — and
    only a very large one is split; as a backstop for a server or proxy that still rejects a body,
    it halves a batch and retries on a `413`. When a capture does split, each part is a separate
    run/report, surfaced in the CLI output.

> Note: the `protocol/upload-request.schema.json` JSON envelope (client/source/redaction/payload)
> is still **DRAFT** and not yet sent on the wire — both clients currently POST the raw gzip with
> query params. The `redact.Summary` already exposes `ruleset_version`/`applied` for that envelope
> when the server adopts it.

## Provenance

- The capture concept is ported from the original `aiscan` scanner's `internal/detectors/*`
  — adapted to collect *raw* artifacts rather than parse them into stats.
- Redact is new (the original never redacted on-device).
- Self-update/tray will be reused from the `sx` client when those steps land.

Per repo rules, all of this is **copied/ported in and cleaned**, never imported as a private
module — this repo is public.

## Known gap: no user-controlled exclusion

A customer expectation has been voiced that employees can **tag work as private/personal so
it is not included in the study**. No such mechanism exists yet — capture collects every
session in the window. The only place a "don't even send this" guarantee can be honored is
**client-side, in capture, before upload** (a user-controlled marker, e.g. an ignore file or
per-project opt-out, that makes matching sessions never enter the dump). Tracked separately;
flagged here because it lands squarely in this layer.
