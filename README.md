# aiscan clients

Open-source client apps for **aiscan** — they help a team understand how it actually uses AI.

A client **captures** local AI-tool usage on your own machine, **redacts** it, and **uploads**
the result to the aiscan server, which produces the analysis. The clients are open source on
purpose: you can read exactly what is captured, what is stripped, and what leaves your machine.
See [`docs/transparency.md`](docs/transparency.md).

## What's here

| Path                       | What it is                                                                                              |
|----------------------------|---------------------------------------------------------------------------------------------------------|
| [`desktop/`](desktop/)     | Desktop client — a Go CLI + background agent with a system-tray UI. Self-updates.                       |
| [`extension/`](extension/) | Browser extension — captures web AI usage (ChatGPT, Claude.ai; Gemini planned). One codebase, Chrome + Firefox. |
| [`protocol/`](protocol/)   | The client↔server contract (upload request/response shapes) shared by both clients.                     |
| [`docs/`](docs/)           | Public transparency docs: what is captured, redacted, uploaded, and stored.                             |

## Principles

- **Thin client.** Clients capture and upload; all parsing, analysis, and reporting happen
  server-side. The client stays small and stable.
- **Redact on the device.** A conservative redaction pass strips obvious secrets before
  anything is uploaded.
- **Store nothing raw.** The server analyzes uploads in memory and persists only comfort-safe
  output. The client surfaces what was sent.
- **Legible.** The desktop tray (and the extension popup) always show status and what was last
  uploaded.

## Usage

You need access to an **aiscan instance** with the `AISCAN` capability enabled for your
org. The default instance is `https://app.skills.new`; point either client at your own with
the instance flag/setting shown below. Both clients authorize with **device-code OAuth** — you
approve once in the browser, and only a short-lived per-user token is stored on your machine.
No password, API key, or secret is ever embedded.

Pick the client that matches where you use AI:

- **Desktop client** — analyzes local CLI sessions: **Claude Code** (`~/.claude/projects`) and
  **Codex** (`~/.codex/sessions`). Whatever is present is captured; a mixed capture uploads as a
  single scan.
- **Browser extension** — analyzes **web** AI usage (claude.ai, chatgpt.com).

### Desktop client (`aiscan`)

Build and install the binary (Go 1.23+), then run it:

    make install                       # builds + installs `aiscan` to ~/.local/bin
    # ensure ~/.local/bin is on your PATH (make install warns if not)

    aiscan login                       # authorize this machine (opens a browser to approve)
    aiscan run                         # capture → redact → upload, then prints a report link

`aiscan run` does the whole pipeline in one shot (it also authorizes on first use, so `login`
is optional). Useful flags:

    aiscan run --window-days 7         # only sessions modified in the last 7 days (0 = all)
    aiscan run --instance https://my-instance.example.com   # target a non-default instance

Want to see exactly what would be sent **before** uploading anything? `capture` is read-only
and never uploads:

    aiscan capture --window-days 7                 # print a per-source + redaction summary
    aiscan capture --out /tmp/dump                 # also write the redacted dump to inspect
    aiscan capture --show-redactions               # debug: list every redacted match

Every run prints the **trust surface**: how many artifacts were collected and exactly what
redaction stripped (e.g. `redacted: 27 (email 2, sk-key 18, …)`). The cached token lives at
`<config-dir>/aiscan/token.json` (owner-only); delete it or re-run `login` to re-authorize.

### Browser extension

No build step — it's plain Manifest V3 JavaScript. Load it unpacked:

- **Chrome:** `chrome://extensions` → enable **Developer mode** → **Load unpacked** → select the
  [`extension/`](extension/) folder.
- **Firefox:** `about:debugging` → **This Firefox** → **Load Temporary Add-on** → pick
  `extension/manifest.json`.

Then:

1. Make sure you're **logged in** to claude.ai / chatgpt.com in the same browser.
2. **Refresh** the tab (content scripts inject on load).
3. Click the on-page **⚙** (bottom-right) to set the instance URL if it isn't
   `https://app.skills.new`. A different instance must also be added to the manifest's
   `host_permissions`.
4. Click the orange **"aiscan: scan N"** button → approve the one-time OAuth tab → the panel
   links you to the report.

See [`extension/SPIKE.md`](extension/SPIKE.md) for the capture details.

## Developing

Requires Go 1.23+ and Node (for the extension tests). Everything runs through the
single root Makefile (`make help` lists all targets):

    make test            # run all tests (Go + JS)
    make lint            # go vet
    make prepush         # format check + lint + all tests
    make build           # build the `aiscan` binary into desktop/bin
    make run ARGS="run --window-days 7"   # run a verb without installing

See [`desktop/`](desktop/) and [`extension/`](extension/) for client-specific docs.

## Status

**Browser extension:** working end-to-end (claude.ai + chatgpt.com → redact → upload).
**Desktop client:** `login` / `capture` / `run` implemented (Claude Code capture, on-device
redaction, device-code auth, and upload). The background agent, system tray, self-update, and
additional capture sources (e.g. Cursor) are still to come.

## License

TBD — to be chosen before this repository is made public.
