// Background worker — packs the captured conversations and uploads them to the
// configured Pulse aiscan instance.
//
// The capture itself runs in the content script (content.js) on the AI site's
// page, where fetch carries first-party credentials. This worker does the parts
// that must NOT run on the site's origin (they target the aiscan instance, a
// different origin):
//   1. ask the server which spans it still needs (aiscanSyncPlan over GraphQL),
//   2. pack each provider's raw capture into a gzipped tar (one file per
//      conversation, under that provider's dir),
//   3. obtain an OAuth access token via the device-code flow (cached),
//   4. POST the gzip to {instance}/api/aiscan/ingest as evidence for one span.
//
// This mirrors the desktop client's v1 sync contract: the client discovers what
// exists, the server hands back the missing spans, and the client uploads only
// those — so repeat syncs never re-fetch or re-upload transcripts the server
// already has. Each upload deposits evidence for its span; the analysis report
// is built server-side, separately.
//
// Parsing is the SERVER's job: every provider uploads its raw API capture and a
// dedicated server-side parser turns it into the normalized session model. The
// client stays thin — no transcoding here. There is no local daemon anymore.

const DEFAULT_INSTANCE = "https://app.skills.new";
const AISCAN_CLIENT_ID = "sleuth-aiscan"; // well-known public client on the server
const OAUTH_SCOPE = "skills";
const DEVICE_GRANT_TYPE = "urn:ietf:params:oauth:grant-type:device_code";
// v1 sync contract — same schema version the desktop client declares. It scopes
// both the sync-plan query and the ingest upload so the server pairs a plan with
// the evidence it later receives.
const SCHEMA_VERSION = 1;

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

async function getInstanceUrl() {
  const { config } = await chrome.storage.local.get("config");
  const raw = (config && config.instanceUrl) || DEFAULT_INSTANCE;
  return raw.replace(/\/+$/, ""); // no trailing slash
}

// ---------------------------------------------------------------------------
// tar + gzip — the upload body is a gzipped tar, one file per conversation
// under its provider dir (e.g. claude-web/<uuid>.json). The server only
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
  if (
    auth &&
    auth.instanceUrl === instanceUrl &&
    auth.accessToken &&
    auth.expiresAt > Date.now() + 60_000
  ) {
    return auth.accessToken;
  }
  return null;
}

async function storeToken(instanceUrl, accessToken, expiresIn) {
  const expiresAt = Date.now() + Math.max(0, (expiresIn || 3600) - 60) * 1000;
  await chrome.storage.local.set({
    auth: { instanceUrl, accessToken, expiresAt },
  });
}

async function startDeviceAuthorization(instanceUrl) {
  const res = await fetch(instanceUrl + "/api/oauth/device-authorization/", {
    method: "POST",
    headers: { "content-type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({
      client_id: AISCAN_CLIENT_ID,
      scope: OAUTH_SCOPE,
    }),
  });
  const text = await res.text();
  if (!res.ok)
    throw new Error(
      "device authorization failed (" + res.status + "): " + text,
    );
  return JSON.parse(text);
}

async function pollForToken(
  instanceUrl,
  deviceCode,
  intervalSec,
  expiresInSec,
) {
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
  throw new Error(
    "authorization timed out — approve the request and scan again",
  );
}

async function ensureToken(instanceUrl, tabId) {
  const cached = await getCachedToken(instanceUrl);
  if (cached) return cached;

  const auth = await startDeviceAuthorization(instanceUrl);
  // Prefer the server's complete URI (code already embedded). If it only gives
  // a plain verification_uri, synthesize the complete form by appending the
  // user_code query param (RFC 8628 convention) so the approval page can
  // prefill it — the user just clicks "Authorize" instead of pasting a code.
  let verifyUrl = auth.verification_uri_complete;
  if (!verifyUrl && auth.verification_uri) {
    verifyUrl = auth.verification_uri;
    if (auth.user_code) {
      verifyUrl +=
        (verifyUrl.includes("?") ? "&" : "?") +
        "user_code=" +
        encodeURIComponent(auth.user_code);
    }
  }
  // Open the approval page; pass the code along only as a fallback to display.
  if (verifyUrl) chrome.tabs.create({ url: verifyUrl });
  if (tabId != null) {
    chrome.tabs
      .sendMessage(tabId, {
        type: "authPrompt",
        userCode: auth.user_code,
        verifyUrl,
      })
      .catch(() => {});
  }

  const token = await pollForToken(
    instanceUrl,
    auth.device_code,
    auth.interval,
    auth.expires_in,
  );
  await storeToken(instanceUrl, token.access_token, token.expires_in);
  return token.access_token;
}

// ---------------------------------------------------------------------------
// Upload: pack -> tar.gz -> authorize -> POST to /api/aiscan/ingest
// ---------------------------------------------------------------------------

// Per-provider packaging: where to file each conversation, the wire "source",
// and how to read its id. Every provider now uploads its RAW capture as JSON;
// the matching server-side parser turns it into sessions. `pack` returns the
// file body, or "" to skip a conversation that didn't capture cleanly.
const rawJson = (c) => (c ? JSON.stringify(c) : "");
const PROVIDER_CFG = {
  "claude-ai": {
    pack: rawJson, // the raw claude.ai conversation object (chat_messages, …)
    dir: "claude-web/",
    source: "claude-web",
    idOf: (c) => c.uuid,
  },
  chatgpt: {
    pack: rawJson, // the raw chatgpt conversation object (mapping tree, …)
    dir: "chatgpt/",
    source: "chatgpt-web",
    idOf: (c) => c.conversation_id || c.id,
  },
  gemini: {
    // content.js already unwrapped the RPC transport; skip a failed payload.
    pack: (c) => (c && c.payload ? JSON.stringify(c) : ""),
    dir: "gemini/",
    source: "gemini-web",
    idOf: (c) => c.conversation_id,
  },
};

// aiscanSyncPlan — the client offers its available span(s) and the server
// returns just the spans it still needs. One fixed query, hand-rolled JSON over
// GraphQL (matching upload's style — no client library). The client does no
// interval math of its own; the server owns coverage.
const PLAN_QUERY =
  "query Plan($source: String!, $schemaVersion: Int!, $available: [AiScanSpanInput!]!) {" +
  " aiscanSyncPlan(source: $source, schemaVersion: $schemaVersion, available: $available) {" +
  " neededSpans { start end } } }";

// plan asks the server which spans of this source's history are still missing.
// `msg.available` is the client's discovered span set ([{start,end}] ISO 8601).
// Returns { ok, neededSpans, reportsUrl }; throws on auth/GraphQL errors so the
// message handler can report them.
async function plan(msg, tabId) {
  const instanceUrl = await getInstanceUrl();
  const cfg = PROVIDER_CFG[msg.provider] || PROVIDER_CFG["claude-ai"];
  const token = await ensureToken(instanceUrl, tabId);

  const res = await fetch(instanceUrl + "/graphql", {
    method: "POST",
    headers: {
      "content-type": "application/json",
      authorization: "Bearer " + token,
    },
    body: JSON.stringify({
      query: PLAN_QUERY,
      variables: {
        source: cfg.source,
        schemaVersion: SCHEMA_VERSION,
        available: Array.isArray(msg.available) ? msg.available : [],
      },
    }),
  });
  const text = await res.text();
  if (res.status === 401) {
    await chrome.storage.local.remove("auth"); // force re-auth next time
    throw new Error(
      "unauthorized — the token was rejected; scan again to re-authorize",
    );
  }
  if (!res.ok) throw new Error("sync plan " + res.status + ": " + text);

  const parsed = JSON.parse(text);
  if (parsed.errors && parsed.errors.length)
    throw new Error(
      "sync plan: " + parsed.errors.map((e) => e.message).join("; "),
    );
  const data = parsed.data && parsed.data.aiscanSyncPlan;
  return {
    ok: true,
    neededSpans: (data && data.neededSpans) || [],
    reportsUrl: instanceUrl + "/aiscan",
  };
}

// upload posts the conversations selected for one span as evidence for that
// span. An empty conversation set is a deliberate confirmed-empty window: we
// still POST (with an empty body) so the server records the span as scanned and
// never asks for it again. captured_start/captured_end come from msg.span.
async function upload(msg, tabId) {
  const instanceUrl = await getInstanceUrl();
  const conversations = Array.isArray(msg.conversations)
    ? msg.conversations
    : [];

  // The content script tags each batch with its provider; we file each raw
  // conversation under that provider's dir (one .json per conversation) and
  // report the matching origin as the session "source" (the report's "Where it
  // happened"). The server picks the parser by dir.
  const cfg = PROVIDER_CFG[msg.provider] || PROVIDER_CFG["claude-ai"];

  const files = [];
  conversations.forEach((conv, i) => {
    const data = cfg.pack(conv);
    if (!data) return;
    const name = cfg.dir + (cfg.idOf(conv) || "conv-" + i) + ".json";
    files.push({ name, data });
  });
  // Conversations handed over yet all unpackable is a real failure — surface it
  // rather than silently marking the span covered. (An intentionally empty span
  // arrives as conversations=[] and packs to zero files, which is fine.)
  if (conversations.length && !files.length)
    throw new Error("nothing to upload (no capturable conversations)");

  // The ingest contract is span-based: it requires the captured window as ISO
  // 8601 [captured_start, captured_end]. content.js passes the span the server
  // asked for; fall back to an all-time span if one wasn't supplied.
  const span = msg.span || {};
  const capturedEnd = span.end || new Date().toISOString();
  const capturedStart = span.start || new Date(0).toISOString();

  const mtime = Math.floor(Date.parse(capturedEnd) / 1000);
  const body = files.length
    ? await gzip(buildTar(files, mtime))
    : new Uint8Array(0);

  const token = await ensureToken(instanceUrl, tabId);
  const url =
    instanceUrl +
    "/api/aiscan/ingest?source=" +
    encodeURIComponent(cfg.source) +
    "&captured_start=" +
    encodeURIComponent(capturedStart) +
    "&captured_end=" +
    encodeURIComponent(capturedEnd) +
    "&schema_version=" +
    SCHEMA_VERSION;
  const res = await fetch(url, {
    method: "POST",
    headers: {
      "content-type": "application/gzip",
      authorization: "Bearer " + token,
    },
    body,
  });
  const text = await res.text();
  if (res.status === 401) {
    await chrome.storage.local.remove("auth"); // force re-auth next time
    throw new Error(
      "unauthorized — the token was rejected; scan again to re-authorize",
    );
  }
  if (!res.ok) throw new Error("ingest " + res.status + ": " + text);

  // The response acknowledges the deposited evidence; there is no per-upload
  // report to link to — reports live at the instance's /aiscan index.
  let evidence = "";
  try {
    evidence = JSON.parse(text).evidence || "";
  } catch (_) {}
  return {
    ok: true,
    sessions: files.length,
    evidence,
    reportsUrl: instanceUrl + "/aiscan",
  };
}

// Register the worker's event listeners only in the extension runtime. Under
// Node (the unit tests below `require` this file) `chrome` is absent, so the
// pure service functions can be exercised without the WebExtension APIs.
if (typeof chrome !== "undefined" && chrome.runtime) {
  // The whole UI (scan button, settings ⚙, status panel) lives on the page
  // itself, injected by content.js — there is no popup. Clicking the toolbar
  // icon just focuses an existing claude.ai/chatgpt.com tab, or opens one.
  chrome.action.onClicked.addListener(async () => {
    const tabs = await chrome.tabs.query({
      url: [
        "https://claude.ai/*",
        "https://chatgpt.com/*",
        "https://gemini.google.com/*",
      ],
    });
    // Some matched tabs (e.g. devtools) can lack a usable id; fall through to
    // opening a fresh tab rather than throwing on an undefined id.
    const tab = tabs.find((t) => t && t.id != null);
    if (tab) {
      await chrome.tabs.update(tab.id, { active: true });
      if (tab.windowId != null)
        await chrome.windows.update(tab.windowId, { focused: true });
    } else {
      await chrome.tabs.create({ url: "https://claude.ai/" });
    }
  });

  chrome.runtime.onMessage.addListener((msg, sender, sendResponse) => {
    const tabId = sender && sender.tab && sender.tab.id;
    const handler =
      msg && msg.type === "plan"
        ? plan
        : msg && msg.type === "upload"
          ? upload
          : null;
    if (!handler) return false;
    handler(msg, tabId)
      .then((r) => sendResponse(r))
      .catch((e) =>
        sendResponse({
          ok: false,
          error: e && e.message ? e.message : String(e),
        }),
      );
    return true; // keep the channel open for the async response
  });
}

// Export the pure service functions for the Node test runner. This block is
// inert in the MV3 worker (no CommonJS `module` there), so it changes nothing
// about how the extension loads.
if (typeof module !== "undefined" && module.exports) {
  module.exports = { buildTar, gzip, plan, upload, PROVIDER_CFG };
}
