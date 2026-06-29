# aiscan client upload schema — v1

> The contract a client implements to sync local AI sessions to the aiscan server. Authoritative
> for client work; the server side lives in `pulse` (`sleuth/apps/aiscan/`). v1 targets the pilot:
> Claude Code (and the other local/transcoded sources below), device-code OAuth, gzip upload.

## Roles split

- **Client** captures local AI sessions and uploads them as **evidence** over time spans it
  declares. That is its whole job: discover what it can read, ask the server what's missing, send
  only that. It does **not** parse, analyze, normalize, or send any identity/role context.
- **Server** owns parsing, the analysis pipeline, report generation, and all gap/policy logic. The
  client does no interval math of its own — the server hands it an exact work-list.

## The sync algorithm

```
sync(source) = upload( aiscanSyncPlan(source, schema_version, available_span(source)).neededSpans )
```

Run **per source**, discovery first:

1. **Discover the available span** — what data actually *exists* for this source (file mtimes, or
   the source's own API), as one or more `[start, end)` spans. Upper bound = now; lower bound = the
   source's earliest data, or unbounded. Report it **raw** — apply no look-back floor and no
   policy; the server applies the floor (currently 90 days) and any schema decision.
2. **Ask for a plan** — call `aiscanSyncPlan` (below) with the source, the client's
   `schema_version`, and the available spans. The server returns the exact `neededSpans` to upload
   (`available − coverage`, floored, schema-scoped). The client never learns whether a schema bump
   or server-side migration happened — it just uploads what it's told.
3. **Upload each needed span** — `POST /api/aiscan/evidence` with the span as the declared
   `[captured_start, captured_end]` and a gzip of the sessions overlapping it. **If a needed span
   has no sessions, still post it with an empty body** — that records a confirmed-empty window so
   the server never asks for it again.

Capture by file **mtime** (not session start) and tolerate overlap: a long session file that gained
turns after an earlier sync is re-uploaded in full, and the server dedups by session id at report
time. Declare the **span** as the window, not the files' own time range.

## REST: `POST /api/aiscan/evidence`

The only binary surface. Streams a multi-MB gzip body, so it stays REST rather than GraphQL.

```
POST /api/aiscan/evidence?source=<source>&captured_start=<iso8601>&captured_end=<iso8601>&schema_version=<int>
Authorization: Bearer <device-code OAuth token>
Content-Type: application/gzip
Body: gzipped tar of the captured sessions, OR empty for a confirmed-empty window

→ 202 {"evidence": "<EV gid>"}
```

- **Query params** carry the metadata so the body stays raw gzip:
  - `source` — one of the source keys below.
  - `captured_start`, `captured_end` — ISO 8601, **timezone-aware**, the declared scanned window.
    `captured_end >= captured_start`. These define coverage; an empty window still covers its range.
  - `schema_version` — the capture schema the client collected under. v1 = `1`. Optional; defaults
    to the server's current schema.
- **Idempotent** on `(org, source, person, captured_start, captured_end, schema_version)` — a
  retried or concurrent upload of the same window returns the existing evidence id, never a
  duplicate. Safe to retry on network failure.
- **No report is started.** Evidence accumulates; reports are generated separately, on demand.
- **Identity:** `person` is the authenticated user (from the token). The client does **not** send
  role, name, or any identity context — that is entered at report-creation time on the server.
- **Errors:** `400` invalid/missing source or window; `401` unauthenticated; `404` aiscan not
  enabled for the org; `413` body over the server's upload cap (~50 MB compressed).

### Tar layout per source

A dump is a gzipped tar. The server parses **by content** — it routes on the top-level directory,
so one dump may carry several trees and they are all parsed. Send the tree(s) for the `source` you
declare:

| `source`        | Top-level dir in the tar         | Notes |
|-----------------|----------------------------------|-------|
| `claude-code`   | `projects/`                      | `~/.claude/projects` — `*.jsonl` transcripts + `*.meta.json` sidecars (incl. `subagents/`). |
| `codex`         | `sessions/`                      | Codex CLI rollout logs. |
| `claude-cowork` | `local-agent-mode-sessions/`     | Pinned to cowork by the dir; the `source` param is ignored for it. |
| `gemini-web`    | `gemini/`                        | Browser-captured `gemini/<id>.json`; pinned by the dir. |
| `claude-web`    | `projects/`                      | Transcode the web conversation into Claude-Code-shaped JSONL; pass `source=claude-web` so it's attributed to the web client. |
| `chatgpt-web`   | `projects/`                      | Same transcode path; pass `source=chatgpt-web`. |

A bare tar with none of these top-level dirs is treated as a `projects/` tree (Claude Code).

## GraphQL: sync plan & coverage

Same bearer token as the upload. Small JSON in/out — no binary.

### `aiscanSyncPlan` — the client's work-list

```graphql
query Plan($source: String!, $schemaVersion: Int!, $available: [AiScanSpanInput!]!) {
  aiscanSyncPlan(source: $source, schemaVersion: $schemaVersion, available: $available) {
    neededSpans { start end }   # exactly what to capture + upload, per source
  }
}
```

`available` is the discovered span set: `[{ start, end }]` (ISO 8601 DateTimes). `person` is the
authenticated user. The server returns `available − coverage` with the look-back floor and schema
scope already applied. Upload each `neededSpan` via the REST endpoint (empty body if no sessions).

### `aiscanCoverage` — what the server already has

```graphql
query Coverage($personIds: [ID!]) {
  aiscanCoverage(personIds: $personIds) {
    person
    source
    intervals { capturedStart capturedEnd schemaVersion sizeBytes }
  }
}
```

Defaults to the authenticated user. Mainly for UIs (e.g. the report builder's gap display); a
client can use it to show "last synced," but the plan query is what drives sync.

## Schema versioning

`schema_version` records *what the client collected*, not how the server derives from it. Bump it
when a new client starts capturing fields older clients didn't. The server scopes coverage to the
client's `schema_version`, so a bump makes still-available history re-enter `neededSpans` and the
client re-uploads it (whatever has aged off the source keeps its old-schema evidence). Whether the
server re-derives from stored raw instead of asking for a re-upload is a server-side decision the
client never sees.

## Auth

Device-code OAuth (RFC 8628); persist the bearer token in the OS keyring. The same token authorizes
both the REST upload and the GraphQL queries.
