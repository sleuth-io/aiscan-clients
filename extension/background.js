// Background worker — turns the captured claude.ai conversations into the
// Claude Code wire format the Pulse aiscan server expects and uploads them
// straight to the configured instance.
//
// The capture itself runs in the content script (content.js) on the claude.ai
// page, where fetch carries first-party cookies. This worker does everything
// that must NOT run on the claude.ai origin:
//   1. transcode each conversation to Claude Code JSONL (one event per line),
//   2. pack the sessions into a gzipped tar under projects/claude-ai/,
//   3. obtain an OAuth access token via the device-code flow (cached),
//   4. POST the gzip to {instance}/api/aiscan/ingest.
//
// There is no local daemon anymore: the extension is the real client.

const DEFAULT_INSTANCE = "http://dev.pulse.sleuth.io";
const AISCAN_CLIENT_ID = "sleuth-aiscan"; // well-known public client on the server
const OAUTH_SCOPE = "skills";
const DEVICE_GRANT_TYPE = "urn:ietf:params:oauth:grant-type:device_code";

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

async function getInstanceUrl() {
  const { config } = await chrome.storage.local.get("config");
  const raw = (config && config.instanceUrl) || DEFAULT_INSTANCE;
  return raw.replace(/\/+$/, ""); // no trailing slash
}

// ---------------------------------------------------------------------------
// claude.ai -> Claude Code JSONL transcode (ported from the Go daemon's
// transcodeConversation). The server has a Claude Code parser only, so web
// conversations are mapped onto Claude Code event lines: user turns carry the
// prompt text; assistant turns carry text/thinking/tool_use blocks so the
// detector counts tool and MCP usage just like a real Claude Code session.
// ---------------------------------------------------------------------------

function firstNonEmpty(...vals) {
  for (const v of vals) if (v) return v;
  return "";
}

function transcodeConversation(conv, capturedAt) {
  const sessionId = conv.uuid;
  if (!sessionId) return "";
  const model = conv.model || "claude-web";
  const fallbackTs = firstNonEmpty(conv.created_at, conv.updated_at, capturedAt);

  const lines = [];
  const messages = Array.isArray(conv.chat_messages) ? conv.chat_messages : [];
  messages.forEach((m, i) => {
    const ts = firstNonEmpty(m.created_at, fallbackTs);
    const entry = { timestamp: ts, sessionId, cwd: "claude.ai" };
    const blocks = Array.isArray(m.content) ? m.content : [];

    if (m.sender === "human" || m.sender === "user") {
      let text = m.text || "";
      if (!text) {
        text = blocks
          .filter((c) => c.type === "text" && c.text)
          .map((c) => c.text)
          .join("\n");
      }
      entry.type = "user";
      entry.message = { role: "user", content: text };
    } else {
      // Assistant turns: map claude.ai blocks onto Claude Code blocks. tool_result
      // is dropped (tool calls are read off the assistant turn).
      const out = [];
      for (const c of blocks) {
        if (c.type === "text" && c.text) out.push({ type: "text", text: c.text });
        else if (c.type === "thinking" && c.thinking) out.push({ type: "thinking", text: c.thinking });
        else if (c.type === "tool_use") out.push({ type: "tool_use", name: c.name, input: c.input });
      }
      if (out.length === 0 && m.text) out.push({ type: "text", text: m.text });
      entry.type = "assistant";
      entry.message = { id: firstNonEmpty(m.uuid, sessionId + "-" + i), model, role: "assistant", content: out };
    }
    lines.push(JSON.stringify(entry));
  });
  return lines.length ? lines.join("\n") + "\n" : "";
}

// ---------------------------------------------------------------------------
// tar + gzip — the upload body is a gzipped tar mirroring ~/.claude/projects,
// with each session at projects/claude-ai/<uuid>.jsonl. The server only
// extracts regular file members, so no directory entries are needed.
// ---------------------------------------------------------------------------

const TAR_BLOCK = 512;

function octalField(value, width) {
  // POSIX ustar numeric field: octal, zero-padded to width-1, then a NUL.
  const s = value.toString(8).padStart(width - 1, "0");
  return s + "\0";
}

function tarHeader(name, size, mtime) {
  const enc = new TextEncoder();
  const header = new Uint8Array(TAR_BLOCK);
  const write = (str, offset, len) => {
    const bytes = enc.encode(str);
    header.set(bytes.subarray(0, len), offset);
  };

  write(name, 0, 100); // name
  write("0000644\0", 100, 8); // mode
  write("0000000\0", 108, 8); // uid
  write("0000000\0", 116, 8); // gid
  write(octalField(size, 12), 124, 12); // size
  write(octalField(mtime, 12), 136, 12); // mtime
  write("        ", 148, 8); // chksum placeholder (8 spaces)
  write("0", 156, 1); // typeflag: regular file
  write("ustar\0", 257, 6); // magic
  write("00", 263, 2); // version

  // Checksum = sum of all header bytes with the chksum field as spaces.
  let sum = 0;
  for (let i = 0; i < TAR_BLOCK; i++) sum += header[i];
  write(sum.toString(8).padStart(6, "0") + "\0 ", 148, 8);
  return header;
}

function buildTar(files, mtime) {
  const enc = new TextEncoder();
  const chunks = [];
  for (const f of files) {
    const data = enc.encode(f.data);
    chunks.push(tarHeader(f.name, data.length, mtime));
    chunks.push(data);
    const rem = data.length % TAR_BLOCK;
    if (rem) chunks.push(new Uint8Array(TAR_BLOCK - rem)); // pad to block
  }
  chunks.push(new Uint8Array(TAR_BLOCK * 2)); // two zero blocks terminate the archive

  const total = chunks.reduce((n, c) => n + c.length, 0);
  const tar = new Uint8Array(total);
  let off = 0;
  for (const c of chunks) {
    tar.set(c, off);
    off += c.length;
  }
  return tar;
}

async function gzip(bytes) {
  const cs = new CompressionStream("gzip");
  const stream = new Response(bytes).body.pipeThrough(cs);
  const buf = await new Response(stream).arrayBuffer();
  return new Uint8Array(buf);
}

// ---------------------------------------------------------------------------
// OAuth device-code flow against the configured instance. The token is cached
// per-instance in chrome.storage until it (nearly) expires.
// ---------------------------------------------------------------------------

async function getCachedToken(instanceUrl) {
  const { auth } = await chrome.storage.local.get("auth");
  if (auth && auth.instanceUrl === instanceUrl && auth.accessToken && auth.expiresAt > Date.now() + 60_000) {
    return auth.accessToken;
  }
  return null;
}

async function storeToken(instanceUrl, accessToken, expiresIn) {
  const expiresAt = Date.now() + Math.max(0, (expiresIn || 3600) - 60) * 1000;
  await chrome.storage.local.set({ auth: { instanceUrl, accessToken, expiresAt } });
}

async function startDeviceAuthorization(instanceUrl) {
  const res = await fetch(instanceUrl + "/api/oauth/device-authorization/", {
    method: "POST",
    headers: { "content-type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({ client_id: AISCAN_CLIENT_ID, scope: OAUTH_SCOPE }),
  });
  const text = await res.text();
  if (!res.ok) throw new Error("device authorization failed (" + res.status + "): " + text);
  return JSON.parse(text);
}

async function pollForToken(instanceUrl, deviceCode, intervalSec, expiresInSec) {
  const deadline = Date.now() + (expiresInSec || 600) * 1000;
  let interval = (intervalSec || 5) * 1000;
  while (Date.now() < deadline) {
    await new Promise((r) => setTimeout(r, interval));
    const res = await fetch(instanceUrl + "/api/oauth/token/", {
      method: "POST",
      headers: { "content-type": "application/x-www-form-urlencoded" },
      body: new URLSearchParams({
        grant_type: DEVICE_GRANT_TYPE,
        device_code: deviceCode,
        client_id: AISCAN_CLIENT_ID,
      }),
    });
    const data = await res.json().catch(() => ({}));
    if (res.ok && data.access_token) return data;
    if (data.error === "authorization_pending") continue;
    if (data.error === "slow_down") {
      interval += 5000;
      continue;
    }
    throw new Error("authorization failed: " + (data.error || res.status));
  }
  throw new Error("authorization timed out — approve the request and scan again");
}

async function ensureToken(instanceUrl, tabId) {
  const cached = await getCachedToken(instanceUrl);
  if (cached) return cached;

  const auth = await startDeviceAuthorization(instanceUrl);
  const verifyUrl = auth.verification_uri_complete || auth.verification_uri;
  // Open the approval page and tell the content script to surface the code.
  if (verifyUrl) chrome.tabs.create({ url: verifyUrl });
  if (tabId != null) {
    chrome.tabs.sendMessage(tabId, { type: "authPrompt", userCode: auth.user_code, verifyUrl }).catch(() => {});
  }

  const token = await pollForToken(instanceUrl, auth.device_code, auth.interval, auth.expires_in);
  await storeToken(instanceUrl, token.access_token, token.expires_in);
  return token.access_token;
}

// ---------------------------------------------------------------------------
// Upload: transcode -> tar.gz -> authorize -> POST to /api/aiscan/ingest
// ---------------------------------------------------------------------------

async function upload(msg, tabId) {
  const instanceUrl = await getInstanceUrl();
  const capturedAt = new Date().toISOString();
  const conversations = Array.isArray(msg.conversations) ? msg.conversations : [];

  const files = [];
  conversations.forEach((conv, i) => {
    const jsonl = transcodeConversation(conv, capturedAt);
    if (!jsonl) return;
    const name = "projects/claude-ai/" + (conv.uuid || "conv-" + i) + ".jsonl";
    files.push({ name, data: jsonl });
  });
  if (!files.length) throw new Error("nothing to upload (no transcodable conversations)");

  const mtime = Math.floor(Date.parse(capturedAt) / 1000);
  const body = await gzip(buildTar(files, mtime));

  const token = await ensureToken(instanceUrl, tabId);
  const windowDays = msg.windowDays || 0;
  const url = instanceUrl + "/api/aiscan/ingest?source=claude-code&window_days=" + windowDays;
  const res = await fetch(url, {
    method: "POST",
    headers: { "content-type": "application/gzip", authorization: "Bearer " + token },
    body,
  });
  const text = await res.text();
  if (res.status === 401) {
    await chrome.storage.local.remove("auth"); // force re-auth next time
    throw new Error("unauthorized — the token was rejected; scan again to re-authorize");
  }
  if (!res.ok) throw new Error("ingest " + res.status + ": " + text);

  let gid = "";
  try {
    gid = JSON.parse(text).run || "";
  } catch (_) {}
  const reportUrl = gid ? instanceUrl + "/aiscan/" + gid : instanceUrl + "/aiscan";
  return { ok: true, reportUrl, sessions: files.length };
}

chrome.runtime.onMessage.addListener((msg, sender, sendResponse) => {
  if (msg && msg.type === "options") {
    chrome.runtime.openOptionsPage();
    return false;
  }
  if (msg && msg.type === "upload") {
    const tabId = sender && sender.tab && sender.tab.id;
    upload(msg, tabId)
      .then((r) => sendResponse(r))
      .catch((e) => sendResponse({ ok: false, error: e && e.message ? e.message : String(e) }));
    return true; // keep the channel open for the async response
  }
  return false;
});
