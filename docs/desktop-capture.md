# Desktop capture architecture

How the desktop client (`desktop/`) collects local AI-tool usage. Covers the *what*
and the *why* — the rationale is not obvious from the code alone.

## Where this fits

The client is **thin**: `capture → redact → upload`. Parsing, normalization, analysis,
and reporting are **all server-side**. The client stays small. **Do not add analysis to the client.**

This doc covers the **capture** step only. Redact and upload exist as stubs today.

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
  redact/redact.go          shared, source-agnostic (stub)
  upload/upload.go          shared, source-agnostic (stub)
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

## Provenance

- The capture concept is ported from the original `aiscan` scanner's `internal/detectors/*`
  — adapted to collect *raw* artifacts rather than parse them into stats.
- Redact is new (the original never redacted on-device).
- Upload/auth/self-update/tray will be reused from the `sx` client when those steps land.

Per repo rules, all of this is **copied/ported in and cleaned**, never imported as a private
module — this repo is public.

## Known gap: no user-controlled exclusion

A customer expectation has been voiced that employees can **tag work as private/personal so
it is not included in the study**. No such mechanism exists yet — capture collects every
session in the window. The only place a "don't even send this" guarantee can be honored is
**client-side, in capture, before upload** (a user-controlled marker, e.g. an ignore file or
per-project opt-out, that makes matching sessions never enter the dump). Tracked separately;
flagged here because it lands squarely in this layer.
