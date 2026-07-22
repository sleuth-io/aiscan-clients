// The extension's tab page — the main UI. Opened (or focused) by clicking the
// toolbar icon; background.js owns that. This page does three things:
//
//   1. Site selection: one checkbox per supported site, defaulted from the
//      last 90 days of browser history (visited → checked). Boxes the user
//      touches are persisted as overrides; untouched ones keep following
//      history on each open.
//   2. Start/cancel a sync ("sync:start" / "sync:cancel" to the background
//      worker, which orchestrates the whole run).
//   3. Render the run. The page holds NO run state of its own — it renders
//      purely from chrome.storage.session.syncState (read on load, then
//      storage.onChanged), so closing and reopening it mid-run just works.
//
// Settings (Pulse instance URL, sign out) also live here, ported from the old
// on-page popover.

const DEFAULT_INSTANCE = "https://app.skills.new";
const HISTORY_WINDOW_MS = 90 * 24 * 60 * 60 * 1000;

// Display order + labels. The background worker owns the start URLs; a host
// checked here is passed by name in sync:start.
const SITES = [
  { host: "claude.ai", label: "claude.ai" },
  { host: "chatgpt.com", label: "chatgpt.com" },
  { host: "gemini.google.com", label: "Gemini" },
];

const STATUS_TEXT = {
  pending: "queued",
  waiting: "opening…",
  scanning: "scanning…",
  done: "done",
  failed: "failed",
  skipped: "skipped",
};

const $ = (id) => document.getElementById(id);

// Same node-building helper as the old content-script UI (no innerHTML).
function el(tag, props, children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v == null) continue;
    if (k === "text") node.textContent = v;
    else if (k === "on")
      for (const [ev, fn] of Object.entries(v)) node.addEventListener(ev, fn);
    else node[k] = v;
  }
  for (const c of [].concat(children == null ? [] : children)) {
    if (c == null) continue;
    node.appendChild(typeof c === "string" ? document.createTextNode(c) : c);
  }
  return node;
}

let cfg = { instanceUrl: DEFAULT_INSTANCE };
// host -> bool. `overrides` is only the boxes the user has touched (persisted);
// `selection` is the effective state rendered in the checkboxes.
let overrides = {};
const selection = {};

// ---- Site selection ------------------------------------------------------

// Whether the user visited this site recently. history.search's `text` param
// is a fuzzy match, so verify the hostname on the results. If the history API
// is unavailable (permission not granted), default to checked — showing an
// extra checkbox beats silently skipping someone's site.
async function visitedRecently(host) {
  try {
    const items = await chrome.history.search({
      text: host,
      startTime: Date.now() - HISTORY_WINDOW_MS,
      maxResults: 25,
    });
    return items.some((it) => {
      try {
        const h = new URL(it.url).hostname;
        return h === host || h.endsWith("." + host);
      } catch (_) {
        return false;
      }
    });
  } catch (_) {
    return true;
  }
}

async function initSelection() {
  const { siteSelection } = await chrome.storage.local.get("siteSelection");
  overrides = siteSelection || {};
  for (const site of SITES) {
    selection[site.host] =
      typeof overrides[site.host] === "boolean"
        ? overrides[site.host]
        : await visitedRecently(site.host);
  }
}

function renderSiteList(running) {
  const list = $("site-list");
  list.textContent = "";
  for (const site of SITES) {
    const box = el("input", {
      type: "checkbox",
      checked: !!selection[site.host],
      disabled: running,
      on: {
        change: () => {
          selection[site.host] = box.checked;
          overrides[site.host] = box.checked;
          chrome.storage.local.set({ siteSelection: overrides });
        },
      },
    });
    list.appendChild(
      el("li", {}, el("label", {}, [box, el("span", { text: site.label })])),
    );
  }
}

// ---- Run control ---------------------------------------------------------

async function startSync(force) {
  const hosts = SITES.map((s) => s.host).filter((h) => selection[h]);
  $("run-note").textContent = "";
  if (!hosts.length) {
    $("run-note").textContent = "Select at least one website.";
    return;
  }
  const resp = await chrome.runtime
    .sendMessage({ type: "sync:start", sites: hosts, force: !!force })
    .catch(() => null);
  if (!resp || !resp.ok) {
    $("run-note").textContent =
      resp && resp.error === "already-running"
        ? "A sync is already running."
        : "Could not start: " + ((resp && resp.error) || "no response");
  }
  // The run itself is rendered from storage as the background updates it.
}

async function cancelSync() {
  await chrome.runtime.sendMessage({ type: "sync:cancel" }).catch(() => null);
}

// ---- Rendering -----------------------------------------------------------

function renderProgress(state) {
  const section = $("progress-section");
  if (!state) {
    section.hidden = true;
    return;
  }
  section.hidden = false;

  // Device-code approval prompt, while the background waits for the token.
  const authNote = $("auth-note");
  authNote.textContent = "";
  if (state.phase === "authorizing") {
    authNote.hidden = false;
    authNote.appendChild(
      document.createTextNode(
        'Authorizing — click "Authorize" in the opened tab. ',
      ),
    );
    if (state.auth && state.auth.verifyUrl)
      authNote.appendChild(
        el("a", {
          href: state.auth.verifyUrl,
          target: "_blank",
          rel: "noopener",
          text: "Open the approval page",
        }),
      );
    if (state.auth && state.auth.userCode)
      authNote.appendChild(
        document.createTextNode(
          " (if asked for a code: " + state.auth.userCode + ")",
        ),
      );
  } else {
    authNote.hidden = true;
  }

  const list = $("progress-list");
  list.textContent = "";
  for (const site of state.sites) {
    const row = el("div", { className: "site-row" }, [
      el("span", { className: "chip " + site.status, text: STATUS_TEXT[site.status] || site.status }),
      el("span", { className: "host", text: site.host }),
      el("span", {
        className: "detail",
        text:
          site.status === "done"
            ? site.synced + " session" + (site.synced === 1 ? "" : "s")
            : site.status === "failed"
              ? site.error || ""
              : "",
      }),
    ]);
    const item = el("li", {}, row);
    if (site.log && site.log.length) {
      const pre = el("pre", { text: site.log.join("\n") });
      const details = el("details", { className: "log" }, [
        el("summary", { text: "Log" }),
        pre,
      ]);
      // Keep the log of whatever is currently running open and scrolled.
      if (site.status === "waiting" || site.status === "scanning")
        details.open = true;
      item.appendChild(details);
      requestAnimationFrame(() => (pre.scrollTop = pre.scrollHeight));
    }
    list.appendChild(item);
  }

  const links = $("result-links");
  links.textContent = "";
  if (state.phase === "error") {
    links.appendChild(
      el("div", { className: "error", text: "Sync failed: " + (state.error || "unknown error") }),
    );
  }
  if (state.phase === "done") {
    links.appendChild(
      el("a", {
        className: "reports",
        href: state.reportsUrl,
        target: "_blank",
        rel: "noopener",
        text: "Open reports",
      }),
    );
    const total = state.sites.reduce((n, s) => n + (s.synced || 0), 0);
    // The escape hatch for missing data, ported from the old on-page UI: only
    // at the dead end (a run that found nothing to send), and one-shot — a
    // force left switched on would re-upload everything on every sync forever.
    if (total === 0 && !state.force) {
      links.appendChild(
        el("a", {
          className: "force",
          href: "#",
          text: "Conversations missing? Send them again",
          on: {
            click: (e) => {
              e.preventDefault();
              startSync(true);
            },
          },
        }),
      );
    }
  }
}

function render(state) {
  const running =
    !!state && (state.phase === "authorizing" || state.phase === "running");
  $("sync-btn").hidden = running;
  $("cancel-btn").hidden = !running;
  renderSiteList(running);
  renderProgress(state);
}

// ---- Settings ------------------------------------------------------------

// Commit only on blur/Enter so transient or invalid mid-edit values never
// become the upload target. Empty falls back to the default; anything that
// isn't a valid http(s) URL reverts to the last saved value.
function commitInstance() {
  const input = $("instance-input");
  const raw = input.value.trim();
  if (!raw) {
    cfg.instanceUrl = DEFAULT_INSTANCE;
  } else {
    let url;
    try {
      url = new URL(raw);
    } catch (_) {
      input.value = cfg.instanceUrl;
      return;
    }
    if (url.protocol !== "http:" && url.protocol !== "https:") {
      input.value = cfg.instanceUrl;
      return;
    }
    cfg.instanceUrl = raw.replace(/\/+$/, "");
  }
  input.value = cfg.instanceUrl;
  chrome.storage.local.set({ config: cfg });
}

function flashSettings(msg) {
  $("settings-note").textContent = msg;
  setTimeout(() => ($("settings-note").textContent = ""), 1500);
}

// ---- Init ----------------------------------------------------------------

async function init() {
  const { config, devSettings } = await chrome.storage.local.get([
    "config",
    "devSettings",
  ]);
  if (config) cfg = Object.assign(cfg, config);

  // Developer settings (instance URL, sign out) are hidden behind a toggle —
  // ordinary users never need them; the default instance just works.
  $("dev-toggle").checked = !!devSettings;
  $("dev-settings").hidden = !devSettings;
  $("dev-toggle").addEventListener("change", (e) => {
    $("dev-settings").hidden = !e.target.checked;
    chrome.storage.local.set({ devSettings: e.target.checked });
  });

  $("instance-input").value = cfg.instanceUrl;
  $("instance-input").addEventListener("change", commitInstance);
  $("instance-input").addEventListener("keydown", (e) => {
    if (e.key === "Enter") e.target.blur();
  });
  $("signout-btn").addEventListener("click", () => {
    chrome.storage.local.remove("auth", () => flashSettings("Signed out."));
  });
  $("sync-btn").addEventListener("click", () => startSync(false));
  $("cancel-btn").addEventListener("click", cancelSync);

  await initSelection();

  const { syncState } = await chrome.storage.session.get("syncState");
  render(syncState || null);

  // Everything after this point is push-rendered from the background's writes.
  chrome.storage.session.onChanged.addListener((changes) => {
    if (changes.syncState) render(changes.syncState.newValue || null);
  });
}

init();
