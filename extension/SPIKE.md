# claude.ai + chatgpt.com + Gemini capture

Pull **all** your claude.ai, chatgpt.com, and gemini.google.com conversations and run aiscan's
analysis over them — the same way the desktop client analyzes Claude Code sessions. The extension
uploads **directly to a Pulse aiscan instance** (`/api/aiscan/ingest`); there is no local Go
daemon in the loop anymore.

The capture flow (list conversations → window-filter → fetch each transcript → upload) is shared;
a **per-site adapter** in `content.js` (keyed by hostname) knows how to list and fetch on each
origin, and `background.js` packs the batch for that provider's server-side parser. Adding a site
is one adapter plus one packer.

Two packaging shapes exist today: claude.ai and chatgpt.com **transcode** to Claude Code JSONL
(the server reuses its Claude Code parser); Gemini is captured **raw** (the content script only
unwraps the RPC transport) and parsed by a dedicated server-side parser. New surfaces should
prefer the raw-capture + server-parser shape — it keeps the client thin and the fragile parsing
testable in Python.

## Pieces

- **`content.js`** — runs on claude.ai / chatgpt.com in the **page's own origin**, so its `fetch`
  carries your first-party session credentials. Injects an on-page button (bottom-right), picks
  the **provider adapter** for the current hostname, enumerates your conversations, fetches each
  transcript, and hands the batch (tagged with the provider name) to the background worker. It
  does no parsing.
- **`background.js`** — the real client. It (1) packs each conversation per `PROVIDER_CFG` —
  claude.ai/chatgpt.com transcode to Claude Code JSONL under `projects/{claude-ai,chatgpt}/<id>.jsonl`,
  Gemini ships its raw payload under `gemini/<id>.json` — into a **gzipped tar** (the wire format
  `/api/aiscan/ingest` expects), (2) gets an OAuth access token via the **device-code flow**
  (well-known client `sleuth-aiscan`, cached in `chrome.storage`), and (3) POSTs the gzip to
  `{instance}/api/aiscan/ingest?source=<claude-web|chatgpt-web|gemini-web>`. The server stores it
  and runs the pipeline on a Celery worker; we get back a run GID and link to its report.
- **Inline settings panel** (in `content.js`) — the on-page **⚙** toggles a floaty popover to set
  the **instance URL** (`http://dev.pulse.sleuth.io` for local dev, `https://app.skills.new` for
  prod), the history window, and to sign out — all without leaving claude.ai.
- There is **no popup** — the entire UI (scan button, settings ⚙, status panel) is injected on the
  claude.ai page. Clicking the toolbar icon just focuses (or opens) a claude.ai tab.

The claude.ai page can't reach the instance directly when it's `http://` (mixed content), and a
content script is bound by the page's CORS — so the cross-origin upload + OAuth calls run in the
background worker, which holds host permissions for the instance.

## Run it

1. Have a Pulse instance running with the `AISCAN` flag enabled for your org (locally:
   `http://dev.pulse.sleuth.io`).
2. Load the extension **unpacked**, logged in to claude.ai in the same browser:
   - **Firefox:** `about:debugging` → This Firefox → Load Temporary Add-on → pick `manifest.json`.
   - **Chrome:** `chrome://extensions` → Developer mode → Load unpacked → pick this folder.
3. **Refresh the claude.ai tab** (content scripts only inject on load). Click the on-page **⚙**
   and set the instance URL (defaults to `https://app.skills.new`).
4. Click the orange **"aiscan: scan N"** button bottom-right. On the first scan an approval tab
   opens — authorize the extension once; the upload then continues automatically and the panel
   links to the report.

## Tests

`background.js`'s service functions (the two transcoders, the tar/gzip packaging, and the
`upload` orchestration) have unit tests with **no build step and no dependencies** — they use the
Node built-in test runner. `background.js` registers its `chrome.*` listeners behind a runtime
guard and exports the pure functions under CommonJS, so Node can `require` it; both shims are
inert in the MV3 worker. Run from `extension/`:

```
node --test        # or: npm test
```

## What we learned (real-account findings)

- **Auth:** the API is cookie-authed, but cookies are first-party/SameSite, so the fetch must
  run in the page (content script), not from the extension origin or the isolated world.
- **Org selection matters:** an account has several orgs; chat endpoints 403 ("Invalid
  authorization for organization") on api-only orgs. Pick the org whose `capabilities` include
  `"chat"`.
- **Message shape:** top-level `text` is usually empty; real content is in `content[]` blocks
  typed `text` / `thinking` / `tool_use` / `tool_result`. Web sessions are full of tool/MCP use
  (web_search, Slack connector, etc.). The transcoder maps these blocks onto Claude Code blocks
  so the existing detector reports tool/MCP usage just like it does for Claude Code.

## What we learned (chatgpt.com)

- **Auth is Bearer, not just cookies.** `/backend-api/*` returns 401 on cookies alone. The page's
  first-party `/api/auth/session` endpoint (cookie-authed) mints a short-lived `accessToken`; the
  adapter fetches it once and sends `Authorization: Bearer …` on every backend call.
- **The list is paginated.** `/backend-api/conversations?offset&limit&order=updated` returns
  `{items, total, limit, offset}`; `limit` accepts at least 100. The adapter walks `offset` until
  it has seen `total`. List items carry `update_time`/`create_time` as ISO strings (used for the
  window filter); the transcript is fetched separately.
- **Transcripts are a tree, not a list.** `/backend-api/conversation/{id}` returns
  `mapping` (node id → `{message, parent, children}`) + `current_node`. Edits/regenerations create
  sibling branches, so we linearize the **active branch** by walking `parent` from `current_node`
  to the root and reversing. `create_time` is float epoch seconds (→ ISO).
- **Message shape.** Each node's `message` has `author.role` (`system`/`user`/`assistant`/`tool`)
  and `content.content_type`: `text` (parts are strings), `thoughts` (gpt-5 reasoning summaries,
  parts are `{summary, content}` → mapped to `thinking`), and `code` with a non-`all` `recipient`
  (a tool call — `recipient` names the tool, `content.text` is the JSON args → `tool_use`). Hidden
  system turns (`is_visually_hidden_from_conversation`) and `tool`-role results are dropped, just
  like claude.ai's `tool_result`.

## What we learned (gemini.google.com)

- **It's `batchexecute`, not REST.** Gemini's web backend speaks Google's `batchexecute` RPC
  (POST `/_/BardChatUi/data/batchexecute?rpcids=<id>`), with obfuscated rpc ids and nested-array
  responses framed by a `)]}'` prefix + length-delimited `wrb.fr` rows. No clean JSON API.
- **Auth is an XSRF token, not a bearer.** Calls need `at` (`SNlM0e`) plus the build label
  (`cfb2h`) and session id (`FdrFJe`). The isolated content world can't read
  `window.WIZ_global_data`, so the adapter regexes them out of the **app HTML** (same-origin fetch
  with cookies) — no main-world injection, which the Trusted-Types CSP would block anyway.
- **Two rpcs.** `MaZiqc` lists conversations (`[id, title, …, [epochSec,nanos], …]` per entry —
  the adapter normalizes the timestamp to ISO for the window filter; only the first page is
  fetched today). `hNvQHb` loads one: `payload[0]` is the turns (newest-first); per turn, the
  user prompt is at `t[2][0][0]`, the assistant markdown at `t[3][0][0][1][0]`, the
  `[epochSec,nanos]` at `t[4]`.
- **Captured raw, parsed server-side.** The adapter unwraps only the RPC transport and uploads the
  inner payload as `gemini/<id>.json` (`source=gemini-web`); the dedicated `gemini_web` parser in
  Pulse walks the nested arrays. No tool calls or token usage are exposed, so a Gemini session is
  prompts + assistant turn-markers (tier `medium`).

## Known spike shortcuts (not the v2 design)

- The extension **transcodes** web conversations → Claude Code JSONL (`transcodeConversation` /
  `transcodeChatGPTConversation` in `background.js`) and uploads them with `source=claude-web` or
  `source=chatgpt-web`. The server reuses the Claude Code parser for the JSONL shape but stamps
  the upload's source onto each session, so the report attributes them correctly. The real v2
  server would expose dedicated versioned `claude-web` / `chatgpt-web` parsers mapping straight to
  the normalized session model.
- No on-device redaction yet; raw transcript text is uploaded (it's PII and stays server-side).
- The claude.ai conversation list isn't paged (chatgpt.com's is).
- claude.ai carries no token usage, so cost/token columns stay zero.
- Per-message model isn't recorded by the API; the conversation-level model is applied to all
  assistant turns.
