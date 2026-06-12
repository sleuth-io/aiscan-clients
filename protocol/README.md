# protocol — the client↔server contract (canonical)

This directory **is** the source of truth for what a client uploads and what the server returns.
It is not a mirror of anything: the desktop client, the browser extension, and the server all
**consume** these schemas — none of them redefine the shapes elsewhere.

- Language-neutral **JSON Schema** (draft 2020-12), so every consumer can validate or generate
  types from the same files:
  - Go (desktop) — generate/validate from the schemas.
  - TypeScript (extension) — generate/validate from the schemas.
  - Python (server) — validate/parse against the schemas.
- The server pulls these schemas from this repo (submodule / vendored-at-build / package) rather
  than hand-copying them. If a shape changes, it changes **here**, once.

## Schemas

- [`upload-request.schema.json`](upload-request.schema.json) — metadata envelope a client sends
  with a redacted capture upload. The redacted dump itself rides as a gzipped payload.
- [`upload-response.schema.json`](upload-response.schema.json) — the server's acknowledgement.
- [`capture-config.schema.json`](capture-config.schema.json) — server-delivered config that
  drives the browser extension's capture (data only; MV3 forbids remote code).

Shapes are **draft** until the first end-to-end upload lands; fields marked accordingly.
