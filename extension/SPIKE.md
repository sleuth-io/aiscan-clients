# claude.ai capture

Pull **all** your claude.ai conversations and run aiscan's analysis over them — the same way the
desktop client analyzes Claude Code sessions. The extension now uploads **directly to a Pulse
aiscan instance** (`/api/aiscan/ingest`); there is no local Go daemon in the loop anymore.

## Pieces

- **`content.js`** — runs on claude.ai in the **page's own origin**, so its `fetch` carries your
  first-party session cookies. Injects an on-page **"aiscan: scan N"** button (bottom-right),
  enumerates your conversations, fetches each transcript, and hands the batch to the background
  worker. It does no parsing.
- **`background.js`** — the real client. It (1) transcodes each conversation to Claude Code JSONL
  (the server has a Claude Code parser only), (2) packs the sessions into a **gzipped tar** under
  `projects/claude-ai/<uuid>.jsonl` — the wire format `/api/aiscan/ingest` expects, (3) gets an
  OAuth access token via the **device-code flow** (well-known client `sleuth-aiscan`, cached in
  `chrome.storage`), and (4) POSTs the gzip to `{instance}/api/aiscan/ingest`. The server stores
  it and runs the pipeline on a Celery worker; we get back a run GID and link to its report.
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

## Known spike shortcuts (not the v2 design)

- The extension **transcodes** claude.ai → Claude Code JSONL (`transcodeConversation` in
  `background.js`) and uploads `source=claude-code`, so web sessions are attributed as Claude
  Code in the report. The real v2 server would expose a dedicated versioned `claude-web` parser
  (and `source` value) mapping straight to the normalized session model.
- No on-device redaction yet; raw transcript text is uploaded (it's PII and stays server-side).
- No pagination on the conversation list endpoint yet.
- claude.ai carries no token usage, so cost/token columns stay zero.
- Per-message model isn't recorded by the API; the conversation-level model is applied to all
  assistant turns.
