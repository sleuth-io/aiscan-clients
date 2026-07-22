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

async function ensureToken(instanceUrl, onPrompt) {
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
  // Open the approval page; hand the prompt (code + link) to the caller so the
  // tab page can render it as a fallback in case the opened tab gets lost.
  if (verifyUrl) chrome.tabs.create({ url: verifyUrl });
  if (onPrompt) {
    try {
      onPrompt({ userCode: auth.user_code || null, verifyUrl: verifyUrl || null });
    } catch (_) {}
  }

  const token = await pollForToken(
    instanceUrl,
    auth.device_code,
    auth.interval,
    auth.expires_in,
  );
  await storeToken(instanceUrl, token.access_token, token.expires_in);
  // Approval done — tell the caller to take the prompt down again.
  if (onPrompt) {
    try {
      onPrompt(null);
    } catch (_) {}
  }
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
// `force` asks the server to ignore what it has already recorded and hand back the
// whole window — the user's escape hatch when their data isn't there. The server
// still clips to its own floor, so this can't reach further back than a normal sync.
const PLAN_QUERY =
  "query Plan($source: String!, $schemaVersion: Int!, $available: [AiScanSpanInput!]!, $force: Boolean) {" +
  " aiscanSyncPlan(source: $source, schemaVersion: $schemaVersion, available: $available, force: $force) {" +
  " neededSpans { start end } } }";

// plan asks the server which spans of this source's history are still missing.
// `msg.available` is the client's discovered span set ([{start,end}] ISO 8601).
// Returns { ok, neededSpans, reportsUrl }; throws on auth/GraphQL errors so the
// message handler can report them.
async function plan(msg) {
  const instanceUrl = await getInstanceUrl();
  const cfg = PROVIDER_CFG[msg.provider] || PROVIDER_CFG["claude-ai"];
  // The token is pre-warmed when a sync starts; this only re-authorizes if it
  // expired mid-run, in which case the prompt goes into the run state so the
  // tab page can show it.
  const token = await ensureToken(instanceUrl, syncAuthPrompt);

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
        force: !!msg.force,
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
async function upload(msg) {
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

  const token = await ensureToken(instanceUrl, syncAuthPrompt);
  const url =
    instanceUrl +
    "/api/aiscan/ingest?source=" +
    encodeURIComponent(cfg.source) +
    "&captured_start=" +
    encodeURIComponent(capturedStart) +
    "&captured_end=" +
    encodeURIComponent(capturedEnd) +
    "&schema_version=" +
    SCHEMA_VERSION +
    // Storing is idempotent on window + content, so re-sending the same conversations
    // would otherwise be accepted and quietly ignored. force tells the server to
    // process them again — without it the escape hatch reports success and does nothing.
    (msg.force ? "&force=1" : "");
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

// ---------------------------------------------------------------------------
// Sync orchestration — the extension tab page (app.js) starts a run; this
// worker walks the selected sites one at a time: open the site in a background
// tab, wait for its content script, tell it to scan, collect its progress,
// close the tab, move on. All run state lives in chrome.storage.session: the
// tab page renders purely from it (closing and reopening the page mid-run just
// re-renders the current state), and this worker re-reads it on every event,
// so an MV3 worker restart mid-run loses nothing — the content script's next
// message wakes the worker, and the watchdog alarm covers the one stretch that
// has no inbound events (the ping poll after opening a tab).
// ---------------------------------------------------------------------------

const SITES = {
  "claude.ai": { url: "https://claude.ai/" },
  "chatgpt.com": { url: "https://chatgpt.com/" },
  // /app is the signed-in chat UI; the bare origin can land on a marketing page
  // without the WIZ payload the content script scrapes its tokens from.
  "gemini.google.com": { url: "https://gemini.google.com/app" },
};

const WATCHDOG_ALARM = "aiscan-watchdog";
const SITE_STALL_MS = 3 * 60_000; // no progress from the current site → fail it
const AUTH_STALL_MS = 10 * 60_000; // approval never came → fail the run
const PING_INTERVAL_MS = 500;
const PING_TIMEOUT_MS = 30_000;
const LOG_CAP = 200; // per-site log lines kept; older ones are dropped

// Whether a new run may start: yes unless one is genuinely in flight. A run
// whose lastProgressAt stopped moving long ago is a corpse (the worker died in
// a way the watchdog hasn't cleaned up yet) and may be taken over.
function canStartSync(state, now) {
  if (!state) return true;
  if (state.phase !== "authorizing" && state.phase !== "running") return true;
  const limit = state.phase === "authorizing" ? AUTH_STALL_MS : SITE_STALL_MS;
  return now - state.lastProgressAt > limit;
}

async function getSyncState() {
  const { syncState } = await chrome.storage.session.get("syncState");
  return syncState || null;
}

// Serialized read-modify-write on the run state. `fn` mutates the state in
// place, or returns false to abort (nothing written, resolves null). Every
// write bumps lastProgressAt — the watchdog's liveness signal. Serialization
// matters because progress messages arrive faster than storage round-trips
// complete; unserialized, two handlers would read the same snapshot and one
// write would silently swallow the other.
let stateWriteChain = Promise.resolve();
function updateSyncState(fn) {
  const run = async () => {
    const state = await getSyncState();
    if (!state || fn(state) === false) return null;
    state.lastProgressAt = Date.now();
    await chrome.storage.session.set({ syncState: state });
    return state;
  };
  const p = stateWriteChain.then(run, run);
  stateWriteChain = p.catch(() => {});
  return p;
}

// During a run the device-code prompt goes into the state for the tab page to
// render (code + approval link); ensureToken calls this again with null once
// the token arrives, taking the prompt down.
function syncAuthPrompt(prompt) {
  return updateSyncState((s) => {
    if (s.phase !== "authorizing" && s.phase !== "running") return false;
    s.auth = prompt;
  });
}

async function startSync(hosts, force) {
  const existing = await getSyncState();
  if (!canStartSync(existing, Date.now()))
    return { ok: false, error: "already-running" };

  const selected = (Array.isArray(hosts) ? hosts : []).filter((h) => SITES[h]);
  if (!selected.length) return { ok: false, error: "no sites selected" };

  const instanceUrl = await getInstanceUrl();
  const syncId =
    "sync-" +
    Date.now().toString(36) +
    "-" +
    Math.random().toString(36).slice(2, 8);
  await chrome.storage.session.set({
    syncState: {
      syncId,
      startedAt: Date.now(),
      lastProgressAt: Date.now(),
      phase: "authorizing",
      auth: null,
      force: !!force,
      error: null,
      reportsUrl: instanceUrl + "/aiscan",
      currentIndex: -1,
      sites: selected.map((host) => ({
        host,
        status: "pending",
        tabId: null,
        synced: 0,
        error: null,
        log: [],
      })),
    },
  });
  await chrome.alarms.create(WATCHDOG_ALARM, { periodInMinutes: 1 });
  // Authorize + run in the background; the caller (the tab page) gets its
  // answer now and follows the run through storage.
  authorizeThenRun(syncId, instanceUrl);
  return { ok: true, syncId };
}

// Authorize up front — before any site tab opens — so the approval tab is the
// only thing the user might have to interact with, and it never pops up
// mid-run between site tabs. The 5s poll interval keeps the worker alive
// throughout (each fetch resets the MV3 idle timer).
async function authorizeThenRun(syncId, instanceUrl) {
  try {
    await ensureToken(instanceUrl, syncAuthPrompt);
  } catch (e) {
    await updateSyncState((s) => {
      if (s.syncId !== syncId || s.phase !== "authorizing") return false;
      s.phase = "error";
      s.auth = null;
      s.error = e && e.message ? e.message : String(e);
    });
    await chrome.alarms.clear(WATCHDOG_ALARM);
    return;
  }
  advance(syncId);
}

// Settle-and-move-on driver: pick the next pending site (flipping the run out
// of "authorizing" on the first call — one write, so a worker restart can't
// land between the phase change and the site pick), or finish the run.
async function advance(syncId) {
  const state = await updateSyncState((s) => {
    if (s.syncId !== syncId) return false;
    if (s.phase !== "running" && s.phase !== "authorizing") return false;
    // One site in flight at a time — a stray second advance (e.g. settleSite's
    // racing the watchdog's resume branch) must not open a second tab.
    if (s.sites.some((x) => x.status === "waiting" || x.status === "scanning"))
      return false;
    s.phase = "running";
    s.auth = null;
    const idx = s.sites.findIndex((x) => x.status === "pending");
    if (idx === -1) {
      s.phase = "done";
      s.currentIndex = -1;
    } else {
      s.currentIndex = idx;
      s.sites[idx].status = "waiting";
    }
  });
  if (!state) return;
  if (state.phase === "done") {
    await chrome.alarms.clear(WATCHDOG_ALARM);
    return;
  }
  openAndScan(
    syncId,
    state.currentIndex,
    state.sites[state.currentIndex].host,
    state.force,
  );
}

async function openAndScan(syncId, idx, host, force) {
  let tab;
  try {
    // active:false — capture runs without stealing focus from the user.
    tab = await chrome.tabs.create({ url: SITES[host].url, active: false });
  } catch (e) {
    return settleSite(syncId, idx, { ok: false, error: "could not open tab" });
  }
  const bound = await updateSyncState((s) => {
    if (s.syncId !== syncId || s.sites[idx].status !== "waiting") return false;
    s.sites[idx].tabId = tab.id;
  });
  if (!bound) {
    // The run was cancelled/superseded while the tab opened — don't leak it.
    try {
      await chrome.tabs.remove(tab.id);
    } catch (_) {}
    return;
  }

  // The content script declares run_at document_idle; poll until it answers.
  // The poll's own API calls keep the worker alive; if the worker dies anyway,
  // the watchdog fails this site once progress stalls.
  const deadline = Date.now() + PING_TIMEOUT_MS;
  let ready = false;
  while (Date.now() < deadline) {
    const cur = await getSyncState();
    if (!cur || cur.syncId !== syncId || cur.sites[idx].status !== "waiting")
      return;
    try {
      const r = await chrome.tabs.sendMessage(tab.id, { type: "ping" });
      if (r && r.ready) {
        ready = true;
        break;
      }
    } catch (_) {}
    await new Promise((res) => setTimeout(res, PING_INTERVAL_MS));
  }
  if (!ready)
    return settleSite(syncId, idx, {
      ok: false,
      error: "the page never finished loading",
    });

  let ack = null;
  try {
    ack = await chrome.tabs.sendMessage(tab.id, {
      type: "scan:start",
      syncId,
      force,
    });
  } catch (_) {}
  if (!ack || !ack.ok)
    return settleSite(syncId, idx, {
      ok: false,
      error: (ack && ack.error) || "the scan did not start",
    });
  await updateSyncState((s) => {
    if (s.syncId !== syncId || s.sites[idx].status !== "waiting") return false;
    s.sites[idx].status = "scanning";
  });
}

// Terminal transition for one site: mark it, close its tab, move on. The
// status guard makes settling idempotent — scan:done, the watchdog, and
// tabs.onRemoved can race, and only the first one acts.
async function settleSite(syncId, idx, outcome, expectedTabId) {
  let tabId = null;
  const state = await updateSyncState((s) => {
    if (s.syncId !== syncId) return false;
    const site = s.sites[idx];
    if (!site || (site.status !== "waiting" && site.status !== "scanning"))
      return false;
    if (expectedTabId != null && site.tabId !== expectedTabId) return false;
    tabId = site.tabId;
    site.tabId = null;
    site.status = outcome.ok ? "done" : "failed";
    site.synced = outcome.synced || 0;
    site.error = outcome.ok ? null : outcome.error || "failed";
    site.log.push(
      outcome.ok
        ? "Done — " + (outcome.synced || 0) + " sessions"
        : "Failed: " + (outcome.error || "unknown"),
    );
  });
  if (!state) return;
  if (tabId != null) {
    try {
      await chrome.tabs.remove(tabId);
    } catch (_) {}
  }
  advance(syncId);
}

async function cancelSync() {
  let openTabId = null;
  await updateSyncState((s) => {
    if (s.phase !== "authorizing" && s.phase !== "running") return false;
    s.phase = "cancelled";
    s.auth = null;
    for (const site of s.sites) {
      if (site.status === "waiting" || site.status === "scanning")
        openTabId = site.tabId;
      if (site.status !== "done" && site.status !== "failed") {
        site.status = "skipped";
        site.tabId = null;
      }
    }
  });
  await chrome.alarms.clear(WATCHDOG_ALARM);
  if (openTabId != null) {
    try {
      await chrome.tabs.remove(openTabId);
    } catch (_) {}
  }
  return { ok: true };
}

async function handleProgress(msg, sender) {
  const senderTabId = sender && sender.tab && sender.tab.id;
  await updateSyncState((s) => {
    if (s.syncId !== msg.syncId) return false;
    const site = s.sites[s.currentIndex];
    if (!site || site.tabId !== senderTabId) return false;
    site.log.push(String(msg.line));
    if (site.log.length > LOG_CAP) site.log.splice(0, site.log.length - LOG_CAP);
  });
  return { ok: true };
}

async function handleScanDone(msg, sender) {
  const senderTabId = sender && sender.tab && sender.tab.id;
  const state = await getSyncState();
  if (!state || state.syncId !== msg.syncId) return { ok: true };
  await settleSite(
    msg.syncId,
    state.currentIndex,
    { ok: !!msg.ok, synced: msg.synced || 0, error: msg.error },
    senderTabId,
  );
  return { ok: true };
}

// Alarm-driven liveness check. Alarms (unlike setTimeout) survive worker
// restarts, so a run whose worker died mid-flight still gets cleaned up: a
// stalled site is failed and the run moves on; if the worker died in the gap
// between settling a site and advancing, advance() resumes the walk.
async function watchdogTick() {
  const state = await getSyncState();
  if (!state || (state.phase !== "running" && state.phase !== "authorizing")) {
    await chrome.alarms.clear(WATCHDOG_ALARM);
    return;
  }
  const now = Date.now();
  if (state.phase === "authorizing") {
    if (now - state.startedAt > AUTH_STALL_MS) {
      await updateSyncState((s) => {
        if (s.syncId !== state.syncId || s.phase !== "authorizing")
          return false;
        s.phase = "error";
        s.auth = null;
        s.error = "authorization timed out — sync again to retry";
      });
      await chrome.alarms.clear(WATCHDOG_ALARM);
    }
    return;
  }
  // A pending auth prompt during "running" means a token expired mid-run and
  // plan/upload are waiting on the approval page — give that the same long
  // window the up-front authorization gets, not the site-stall one.
  const stallLimit = state.auth ? AUTH_STALL_MS : SITE_STALL_MS;
  if (now - state.lastProgressAt <= stallLimit) return;
  const site = state.sites[state.currentIndex];
  if (site && (site.status === "waiting" || site.status === "scanning")) {
    await settleSite(state.syncId, state.currentIndex, {
      ok: false,
      error: "stalled — no progress for 3 minutes",
    });
  } else {
    // Settled but never advanced (worker died in between) — resume the walk.
    advance(state.syncId);
  }
}

// One dispatcher for every message the worker answers, so the Node tests can
// drive the whole orchestration without a chrome.runtime listener.
const MESSAGE_TYPES = new Set([
  "plan",
  "upload",
  "sync:start",
  "sync:cancel",
  "scan:progress",
  "scan:done",
]);

function handleMessage(msg, sender) {
  switch (msg.type) {
    case "plan":
      return plan(msg);
    case "upload":
      return upload(msg);
    case "sync:start":
      return startSync(msg.sites, msg.force);
    case "sync:cancel":
      return cancelSync();
    case "scan:progress":
      return handleProgress(msg, sender);
    case "scan:done":
      return handleScanDone(msg, sender);
  }
}

// Register the worker's event listeners only in the extension runtime. Under
// Node (the unit tests below `require` this file) `chrome` is absent, so the
// pure service functions can be exercised without the WebExtension APIs.
if (typeof chrome !== "undefined" && chrome.runtime) {
  // Toolbar icon → the extension's own tab page (app.html): focus it if one is
  // already open, otherwise open it. (No default_popup in the manifest, so the
  // click reaches this listener.)
  chrome.action.onClicked.addListener(async () => {
    const url = chrome.runtime.getURL("app.html");
    const tabs = await chrome.tabs.query({ url });
    // Some matched tabs (e.g. devtools) can lack a usable id; fall through to
    // opening a fresh tab rather than throwing on an undefined id.
    const tab = tabs.find((t) => t && t.id != null);
    if (tab) {
      await chrome.tabs.update(tab.id, { active: true });
      if (tab.windowId != null)
        await chrome.windows.update(tab.windowId, { focused: true });
    } else {
      await chrome.tabs.create({ url });
    }
  });

  chrome.runtime.onMessage.addListener((msg, sender, sendResponse) => {
    if (!msg || !MESSAGE_TYPES.has(msg.type)) return false;
    handleMessage(msg, sender)
      .then((r) => sendResponse(r))
      .catch((e) =>
        sendResponse({
          ok: false,
          error: e && e.message ? e.message : String(e),
        }),
      );
    return true; // keep the channel open for the async response
  });

  // The user closing the current site's tab mid-scan fails that site and moves
  // on (settleSite's status guard makes this a no-op for tabs we closed
  // ourselves — their site is already settled by then).
  chrome.tabs.onRemoved.addListener(async (tabId) => {
    const state = await getSyncState();
    if (!state || state.phase !== "running") return;
    const site = state.sites[state.currentIndex];
    if (site && site.tabId === tabId)
      settleSite(state.syncId, state.currentIndex, {
        ok: false,
        error: "the tab was closed",
      });
  });

  chrome.alarms.onAlarm.addListener((alarm) => {
    if (alarm.name === WATCHDOG_ALARM) watchdogTick();
  });
}

// Export the pure service functions for the Node test runner. This block is
// inert in the MV3 worker (no CommonJS `module` there), so it changes nothing
// about how the extension loads.
if (typeof module !== "undefined" && module.exports) {
  module.exports = {
    buildTar,
    gzip,
    plan,
    upload,
    PROVIDER_CFG,
    SITES,
    canStartSync,
    startSync,
    cancelSync,
    handleMessage,
    getSyncState,
    watchdogTick,
  };
}
