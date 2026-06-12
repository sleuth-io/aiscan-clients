# What the aiscan client does with your data

This document describes exactly what the client collects, what it removes, and what leaves your
machine. The client is open source so these claims are auditable in the code.

## What is captured (locally)

- Records of local AI-tool usage — e.g. coding-assistant session logs, the tools and
  customizations you have installed, and (for the browser extension) AI web conversations.
- Capture is **read-only**: the client does not modify your files or settings.

## What is redacted (before anything leaves the machine)

A conservative redaction pass runs locally and strips obvious secrets — environment variables,
key-shaped strings, and (configurably) file contents — before upload.

## What is uploaded

- The redacted capture is sent to the aiscan server, which parses and analyzes it.
- Uploading content to a server for AI analysis is inherent to this kind of product (any AI
  analysis sends content to a model). The guarantee that matters is what is **kept**.

## What is stored

- The server processes uploads **in memory** and persists **only** comfort-safe output —
  categorized, shareable summaries. Raw captured content is **not stored**.

## How you can see it

- The desktop tray (and the browser popup) show status and let you view what was last uploaded.

## Updates

The desktop client keeps itself up to date by downloading signed, checksum-verified releases
and restarting itself. The browser extension updates through the browser's extension store.
