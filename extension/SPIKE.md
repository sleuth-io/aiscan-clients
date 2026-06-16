# claude.ai capture spike

Cheapest local loop to prove we can pull **all** your claude.ai conversations and run
aiscan's analysis over them — the same way the desktop client analyzes Claude Code sessions.
**Status: working end to end** (verified in Firefox against a real account).

## Pieces

- **This extension** — one build, loads in Chrome and Firefox. It's just an *authenticated
  bridge*; it does no parsing.
  - `content.js` — runs on claude.ai in the **page's own origin**, so its `fetch` carries your
    first-party session cookies. Injects an on-page **"aiscan: scan N"** button (bottom-right),
    enumerates your conversations, fetches each transcript, and hands the batch to the
    background worker.
  - `background.js` — receives the batch and POSTs it to the local daemon (the https claude.ai
    page can't reach `http://127.0.0.1` directly — mixed content — so the worker does it).
  - `popup.html` / `popup.js` — leftover trigger/status UI; the on-page button is the real one.
- **`aiscan serve --local`** (in the `aiscan` repo, `internal/cli/serve.go`) — a loopback HTTP
  receiver that transcodes each web conversation into the Claude Code session shape and runs the
  existing scan/report pipeline.

## Run it

1. Start the receiver (from the `aiscan` repo):
   ```
   go run ./cmd/aiscan serve --local        # listens on 127.0.0.1:8765
   ```
2. Load the extension **unpacked**, logged in to claude.ai in the same browser:
   - **Firefox:** `about:debugging` → This Firefox → Load Temporary Add-on → pick `manifest.json`.
   - **Chrome:** `chrome://extensions` → Developer mode → Load unpacked → pick this folder.
3. **Refresh the claude.ai tab** (content scripts only inject on load), then click the orange
   **"aiscan: scan N"** button bottom-right. Progress shows in the on-page panel; the full report
   prints to the daemon console.

The raw upload is also saved to `~/.aiscan-spike/last-upload.json` for inspection / replay
(`curl --data-binary @… http://127.0.0.1:8765/ingest`).

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

- The receiver **transcodes** claude.ai → Claude Code JSONL to reuse 100% of existing analysis.
  The real v2 server would have a dedicated versioned `claude-web` parser mapping straight to the
  normalized session model. `transcodeConversation` in `serve.go` is where that slots in.
- Capped to the newest N conversations (`MAX_CONVERSATIONS` in `content.js`; set 0 for all).
  No pagination on the list endpoint yet, no redaction, no auth on the daemon.
- claude.ai carries no token usage, so cost/token columns stay zero.
- Per-message model isn't recorded by the API; the conversation-level model is applied to all
  assistant turns.
