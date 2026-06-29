// Background worker — packs the captured conversations and uploads them to the
// configured Pulse aiscan instance.
//
// The capture itself runs in the content script (content.js) on the AI site's
// page, where fetch carries first-party credentials. This worker does the parts
// that must NOT run on the site's origin:
//   1. pack each provider's raw capture into a gzipped tar (one file per
//      conversation, under that provider's dir),
//   2. obtain an OAuth access token via the device-code flow (cached),
//   3. POST the gzip to {instance}/api/aiscan/ingest.
//
// Parsing is the SERVER's job: every provider uploads its raw API capture and a
// dedicated server-side parser turns it into the normalized session model. The
// client stays thin — no transcoding here. There is no local daemon anymore.

const DEFAULT_INSTANCE = "https://app.skills.new";
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

async function upload(msg, tabId) {
  const instanceUrl = await getInstanceUrl();
  const capturedAt = new Date().toISOString();
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
  if (!files.length)
    throw new Error("nothing to upload (no capturable conversations)");

  const mtime = Math.floor(Date.parse(capturedAt) / 1000);
  const body = await gzip(buildTar(files, mtime));

  const token = await ensureToken(instanceUrl, tabId);
  const windowDays = msg.windowDays || 0;
  const url =
    instanceUrl +
    "/api/aiscan/ingest?source=" +
    encodeURIComponent(cfg.source) +
    "&window_days=" +
    windowDays;
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

  let gid = "";
  try {
    gid = JSON.parse(text).run || "";
  } catch (_) {}
  const reportUrl = gid
    ? instanceUrl + "/aiscan/" + gid
    : instanceUrl + "/aiscan";
  return { ok: true, reportUrl, sessions: files.length };
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
    if (msg && msg.type === "upload") {
      const tabId = sender && sender.tab && sender.tab.id;
      upload(msg, tabId)
        .then((r) => sendResponse(r))
        .catch((e) =>
          sendResponse({
            ok: false,
            error: e && e.message ? e.message : String(e),
          }),
        );
      return true; // keep the channel open for the async response
    }
    return false;
  });
}

// Export the pure service functions for the Node test runner. This block is
// inert in the MV3 worker (no CommonJS `module` there), so it changes nothing
// about how the extension loads.
if (typeof module !== "undefined" && module.exports) {
  module.exports = { buildTar, gzip, upload, PROVIDER_CFG };
}
