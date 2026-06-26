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

## Developing

Requires Go 1.23+ and Node (for the extension tests). Everything runs through the
single root Makefile (`make help` lists all targets):

    make test            # run all tests (Go + JS)
    make lint            # go vet
    make prepush         # format check + lint + all tests
    make install         # build + install the `aiscan` binary to ~/.local/bin
    make run ARGS="capture --window-days 7"   # run capture without installing

After `make install`, run the client directly:

    aiscan capture --window-days 7 --out /tmp/dump

See [`desktop/`](desktop/) and [`extension/`](extension/) for client-specific docs.

## Status

Scaffold. Structure and intent are in place; implementations are in progress.

## License

TBD — to be chosen before this repository is made public.
