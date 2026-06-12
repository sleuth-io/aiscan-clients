# protocol — client↔server contract

Shared definitions of what a client uploads and what the server returns, used by both the
desktop client and the browser extension. Keeping it in one place keeps the clients honest
against the server API.

## Scope (to be defined)

- **Auth** — device-code OAuth; clients send a bearer token. No secrets embedded in clients.
- **Upload** — a redacted capture payload (gzipped): the source type/version plus the redacted
  source data. Clients do not normalize; the server owns parsing.
- **Response** — analysis status / result reference.
- **Config** — (browser) the capture-config the server hands the extension.

Treat this as the single source of truth for request/response shapes; generate or mirror types
into `desktop/` and `extension/` from here.
