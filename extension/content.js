// Content script — runs on claude.ai in the page's own origin.
//
// A declared content script auto-runs in both Chrome and Firefox without a
// separate host-permission grant, and its fetch() is same-origin to claude.ai,
// so it carries your first-party session cookies (exactly like running fetch in
// the page console). It injects a small button + status panel; on click it
// pulls the conversations and hands them to the background worker, which
// transcodes, authorizes, and uploads them to the configured Pulse instance
// (the cross-origin upload + OAuth can't run on the claude.ai origin).

(function () {
  if (window.__aiscanInjected) return;
  window.__aiscanInjected = true;

  const CONCURRENCY = 5;
  const DEFAULT_INSTANCE = "https://app.skills.new";
  // Config (history window + Pulse instance) is edited in the inline settings
  // panel below; this is the default until it loads from chrome.storage.
  // windowDays scopes which conversations are sent (0 = all time). instanceUrl
  // is read by background.js to know where to authorize and upload.
  let cfg = { windowDays: 7, instanceUrl: DEFAULT_INSTANCE };

  // ---- Provider adapters -------------------------------------------------
  // claude.ai and chatgpt.com expose different chat APIs, but the scan flow is
  // identical: list conversations -> filter by the history window -> fetch each
  // transcript -> hand the batch to the background worker. Each adapter knows
  // only how to list and fetch on its own origin (where the page's cookies /
  // token apply). The adapter's `name` is sent to background.js so it picks the
  // matching transcoder. Detection is purely by hostname.
  const PROVIDERS = {
    "claude.ai": {
      name: "claude-ai",
      label: "claude.ai",
      async listConversations() {
        // Chat lives under the org with the "chat" capability (see findOrg).
        this.org = await findOrg();
        const list = await getJSON(
          "/api/organizations/" + this.org + "/chat_conversations",
        );
        return (Array.isArray(list) ? list : []).map((c) => ({
          id: c.uuid,
          updated_at: c.updated_at,
          created_at: c.created_at,
        }));
      },
      fetchFull(item) {
        return getJSON(
          "/api/organizations/" +
            this.org +
            "/chat_conversations/" +
            item.id +
            "?tree=True&rendering_mode=messages&render_all_tools=true",
        );
      },
    },
    "chatgpt.com": {
      name: "chatgpt",
      label: "chatgpt.com",
      // Unlike claude.ai, /backend-api is Bearer-authed — cookies alone 401. The
      // token is minted by the first-party /api/auth/session endpoint (cookie
      // auth) and reused for every call in this scan.
      async token() {
        if (!this._token) {
          const s = await getJSON("/api/auth/session");
          this._token = s && s.accessToken;
          if (!this._token) throw new Error("not signed in to chatgpt.com");
        }
        return this._token;
      },
      async listConversations() {
        const headers = { authorization: "Bearer " + (await this.token()) };
        // The list endpoint is paged; walk offset until we've seen `total`.
        const limit = 100;
        let offset = 0;
        const all = [];
        for (;;) {
          const j = await getJSON(
            "/backend-api/conversations?offset=" +
              offset +
              "&limit=" +
              limit +
              "&order=updated",
            headers,
          );
          const items = (j && j.items) || [];
          all.push(...items);
          offset += items.length;
          if (!items.length || offset >= ((j && j.total) || all.length)) break;
        }
        return all.map((c) => ({
          id: c.id,
          updated_at: c.update_time,
          created_at: c.create_time,
        }));
      },
      async fetchFull(item) {
        const headers = { authorization: "Bearer " + (await this.token()) };
        return getJSON("/backend-api/conversation/" + item.id, headers);
      },
    },
  };
  const provider = PROVIDERS[location.hostname] || PROVIDERS["claude.ai"];

  const btn = document.createElement("button");
  btn.textContent = "Scan and upload my chats";
  // The window scope no longer fits the label (the main button now reads as a
  // plain action), so surface it in the tooltip instead — refreshed after Save.
  function updateLabel() {
    btn.title =
      cfg.windowDays > 0
        ? "Scans your " +
          provider.label +
          " sessions from the last " +
          cfg.windowDays +
          " days"
        : "Scans all your " + provider.label + " sessions";
  }
  updateLabel();

  const gear = document.createElement("button");
  gear.textContent = "⚙";
  gear.title = "aiscan settings (instance + history window)";

  // Inline settings panel — a floaty popover anchored above the gear, so the
  // user never leaves claude.ai to edit the instance URL / history window or to
  // sign out. Built from DOM nodes (not innerHTML) because claude.ai enforces a
  // Trusted-Types CSP that blocks string-to-HTML assignment. Reads/writes the
  // same chrome.storage.local "config" that content.js loads below and that
  // background.js reads on upload; "Sign out" clears the cached OAuth token.
  const settings = document.createElement("div");
  settings.style.cssText =
    "position:fixed;z-index:2147483647;bottom:56px;right:16px;width:340px;display:none;" +
    "box-sizing:border-box;padding:12px;background:#1d1d1f;color:#eaeaea;border-radius:8px;" +
    "font:13px system-ui,-apple-system,sans-serif;box-shadow:0 2px 10px rgba(0,0,0,.35)";

  const mkLabel = (text) => {
    const l = document.createElement("div");
    l.textContent = text;
    l.style.cssText = "font-weight:600;margin:0 0 4px";
    return l;
  };
  const mkHint = (text) => {
    const h = document.createElement("div");
    h.textContent = text;
    h.style.cssText = "color:#9a9aa0;font-size:11px;margin:2px 0 10px";
    return h;
  };
  const fieldCss =
    "box-sizing:border-box;padding:5px 7px;border-radius:6px;border:1px solid #3a3a3f;" +
    "background:#111113;color:#eaeaea";

  // Persist immediately on every change — there is no Save button.
  const persist = () => chrome.storage.local.set({ config: cfg });

  // History window — a slider over discrete stops; the last stop (0) is all time.
  const WINDOW_STEPS = [7, 14, 30, 60, 90, 180, 0];
  const WINDOW_TICKS = ["7d", "14d", "30d", "60d", "90d", "180d", "All"];
  const windowText = (d) => (d > 0 ? "Last " + d + " days" : "All time");

  settings.appendChild(mkLabel("History window"));
  const windowValue = document.createElement("div");
  windowValue.style.cssText = "color:#da7756;font-weight:600;margin:-2px 0 6px";
  settings.appendChild(windowValue);
  const slider = document.createElement("input");
  slider.type = "range";
  slider.min = "0";
  slider.max = String(WINDOW_STEPS.length - 1);
  slider.step = "1";
  slider.style.cssText = "width:100%;accent-color:#da7756;cursor:pointer;margin:0";
  settings.appendChild(slider);
  const ticks = document.createElement("div");
  ticks.style.cssText =
    "display:flex;justify-content:space-between;color:#9a9aa0;font-size:10px;margin-top:2px";
  WINDOW_TICKS.forEach((t) => {
    const s = document.createElement("span");
    s.textContent = t;
    ticks.appendChild(s);
  });
  settings.appendChild(ticks);
  const windowHint = mkHint(
    "How far back to scan your " + provider.label + " sessions.",
  );
  settings.appendChild(windowHint);

  slider.addEventListener("input", () => {
    cfg.windowDays = WINDOW_STEPS[parseInt(slider.value, 10)] || 0;
    windowValue.textContent = windowText(cfg.windowDays);
    updateLabel();
    persist();
  });

  // Pulse instance — dev only. Commits on blur/Enter, not per keystroke.
  const instanceHeader = mkLabel("Pulse instance");
  const instanceHint = mkHint(
    "Where uploads go. e.g. http://dev.pulse.sleuth.io for local dev, https://app.skills.new for production.",
  );
  const instanceEl = document.createElement("input");
  instanceEl.type = "text";
  instanceEl.placeholder = DEFAULT_INSTANCE;
  instanceEl.style.cssText =
    fieldCss + ";width:100%;font:12px ui-monospace,Menlo,monospace";
  settings.appendChild(instanceHeader);
  settings.appendChild(instanceHint);
  settings.appendChild(instanceEl);
  // Commit only on blur/Enter so transient or invalid mid-edit values never
  // become the upload target. Empty falls back to the default; anything that
  // isn't a valid http(s) URL reverts to the last saved value.
  const commitInstance = () => {
    const raw = instanceEl.value.trim();
    if (!raw) {
      cfg.instanceUrl = DEFAULT_INSTANCE;
    } else {
      let url;
      try {
        url = new URL(raw);
      } catch (_) {
        instanceEl.value = cfg.instanceUrl;
        return;
      }
      if (url.protocol !== "http:" && url.protocol !== "https:") {
        instanceEl.value = cfg.instanceUrl;
        return;
      }
      cfg.instanceUrl = raw.replace(/\/+$/, "");
    }
    instanceEl.value = cfg.instanceUrl;
    persist();
  };
  instanceEl.addEventListener("change", commitInstance);
  instanceEl.addEventListener("keydown", (e) => {
    if (e.key === "Enter") instanceEl.blur();
  });

  // Account — Sign out lives behind dev mode (most users never need it).
  const signoutBtn = document.createElement("button");
  signoutBtn.textContent = "Sign out";
  signoutBtn.style.cssText =
    "margin-top:8px;padding:7px 14px;background:#7a3b3b;color:#fff;border:none;border-radius:6px;cursor:pointer;font:13px system-ui";
  const savedNote = document.createElement("span");
  savedNote.style.cssText = "margin-left:10px;color:#7fd18a;font-size:12px";
  settings.appendChild(signoutBtn);
  settings.appendChild(savedNote);
  const signoutHint = mkHint(
    "Authorization happens automatically on your first scan (an approval tab opens). Sign out clears the cached token — use it after switching instances.",
  );
  settings.appendChild(signoutHint);

  const flash = (msg) => {
    savedNote.textContent = msg;
    setTimeout(() => (savedNote.textContent = ""), 1500);
  };
  signoutBtn.addEventListener("click", () => {
    chrome.storage.local.remove("auth", () => flash("Signed out."));
  });

  // Subtle dev-mode toggle: reveals the instance field + all help text.
  const devToggle = document.createElement("label");
  devToggle.style.cssText =
    "display:flex;align-items:center;justify-content:flex-end;gap:6px;margin-top:12px;" +
    "padding-top:10px;border-top:1px solid #2c2c30;color:#6a6a70;font-size:11px;cursor:pointer";
  const devCheck = document.createElement("input");
  devCheck.type = "checkbox";
  devCheck.style.cssText = "margin:0;accent-color:#6a6a70;cursor:pointer";
  const devText = document.createElement("span");
  devText.textContent = "Developer mode";
  devToggle.appendChild(devCheck);
  devToggle.appendChild(devText);
  settings.appendChild(devToggle);
  document.documentElement.appendChild(settings);

  const devOnly = [
    windowHint,
    instanceHeader,
    instanceHint,
    instanceEl,
    signoutBtn,
    savedNote,
    signoutHint,
  ];
  const applyDevMode = () => {
    const on = !!cfg.devMode;
    devCheck.checked = on;
    devOnly.forEach((el) => (el.style.display = on ? "" : "none"));
  };
  devCheck.addEventListener("change", () => {
    cfg.devMode = devCheck.checked;
    applyDevMode();
    persist();
  });

  gear.addEventListener("click", () => {
    const showing = settings.style.display !== "none";
    if (!showing) {
      // Opening settings clears any previous scan log out of the popover.
      panel.style.display = "none";
      panel.textContent = "";
      const idx = Math.max(0, WINDOW_STEPS.indexOf(cfg.windowDays));
      slider.value = String(idx);
      windowValue.textContent = windowText(WINDOW_STEPS[idx]);
      instanceEl.value = cfg.instanceUrl || DEFAULT_INSTANCE;
      applyDevMode();
      savedNote.textContent = "";
    }
    settings.style.display = showing ? "none" : "block";
  });
  // Split button: the wide main part runs the scan; the narrow ⚙ part toggles
  // the settings popover. Both live in one rounded bar with a divider between,
  // so it reads as a single control.
  btn.style.cssText =
    "padding:8px 14px;background:#da7756;color:#fff;border:none;cursor:pointer;" +
    "font:13px system-ui,sans-serif;border-right:1px solid rgba(0,0,0,.18)";
  gear.style.cssText =
    "padding:8px 11px 8px 9px;background:#c4634a;color:#fff;border:none;cursor:pointer;" +
    "font:18px system-ui;line-height:1;display:flex;align-items:center";
  const bar = document.createElement("div");
  bar.style.cssText =
    "position:fixed;z-index:2147483647;bottom:16px;right:16px;display:inline-flex;" +
    "border-radius:8px;overflow:hidden;box-shadow:0 2px 10px rgba(0,0,0,.35)";
  bar.appendChild(btn);
  bar.appendChild(gear);
  document.documentElement.appendChild(bar);

  const panel = document.createElement("div");
  panel.style.cssText =
    "position:fixed;z-index:2147483647;bottom:56px;right:16px;width:340px;max-height:260px;" +
    "overflow:auto;display:none;padding:8px;background:#1d1d1f;color:#eaeaea;border-radius:8px;" +
    "font:11px ui-monospace,Menlo,monospace;white-space:pre-wrap;box-shadow:0 2px 10px rgba(0,0,0,.35)";
  document.documentElement.appendChild(panel);
  // Append as a text node (not textContent +=) so DOM children like the report
  // link aren't wiped on the next log line.
  const log = (m) => {
    panel.style.display = "block";
    panel.appendChild(document.createTextNode(m + "\n"));
    panel.scrollTop = panel.scrollHeight;
  };

  async function getJSON(url, extraHeaders) {
    const r = await fetch(url, {
      credentials: "include",
      headers: Object.assign({ accept: "application/json" }, extraHeaders || {}),
    });
    if (!r.ok) throw new Error(r.status + " " + r.statusText + " — " + url);
    return r.json();
  }

  async function findOrg() {
    const orgs = await getJSON("/api/organizations");
    const list = Array.isArray(orgs) ? orgs : [];
    // Chat conversations live under an org with the "chat" capability. Other
    // orgs (enterprise "api", individual "api_individual") 403 on the chat
    // endpoints. Prefer the user's own chat org.
    const chat = list.filter(
      (o) =>
        o &&
        o.uuid &&
        Array.isArray(o.capabilities) &&
        o.capabilities.includes("chat"),
    );
    if (chat.length > 1) {
      log(
        "  " +
          chat.length +
          ' chat orgs; using "' +
          (chat[0].name || chat[0].uuid) +
          '"',
      );
    }
    if (chat[0]) return chat[0].uuid;
    throw new Error('no organization with the "chat" capability found');
  }

  async function mapLimit(items, limit, fn) {
    const out = new Array(items.length);
    let next = 0;
    const worker = async () => {
      while (next < items.length) {
        const i = next++;
        out[i] = await fn(items[i], i);
      }
    };
    await Promise.all(
      Array.from({ length: Math.min(limit, items.length) }, worker),
    );
    return out;
  }

  async function scan() {
    // Take over the popover: drop the settings view, show a fresh log, and lock
    // the ⚙ so settings can't reopen mid-scan.
    btn.disabled = true;
    gear.disabled = true;
    bar.style.opacity = "0.55";
    settings.style.display = "none";
    panel.textContent = "";
    panel.style.display = "block";
    try {
      log("Listing " + provider.label + " conversations…");
      let list = await provider.listConversations();
      const total = list.length;
      log("  found " + total + " conversations");
      if (!total) return;
      // Keep only conversations active within the window (by last update). Both
      // providers report ISO timestamps here (normalized in their adapters).
      if (cfg.windowDays > 0) {
        const cutoff = Date.now() - cfg.windowDays * 24 * 60 * 60 * 1000;
        list = list.filter((c) => {
          const t = Date.parse(c.updated_at || c.created_at || "");
          return isNaN(t) || t >= cutoff;
        });
        log(
          "  " +
            list.length +
            " of " +
            total +
            " active in last " +
            cfg.windowDays +
            " days",
        );
      }
      if (!list.length) return;

      log("Fetching transcripts…");
      let done = 0;
      const conversations = await mapLimit(list, CONCURRENCY, async (item) => {
        const full = await provider.fetchFull(item);
        done++;
        if (done % 5 === 0 || done === list.length)
          log("  " + done + "/" + list.length);
        return full;
      });

      log("Uploading " + conversations.length + " conversations…");
      const resp = await chrome.runtime.sendMessage({
        type: "upload",
        provider: provider.name,
        conversations,
        windowDays: cfg.windowDays,
      });
      if (resp && resp.ok) {
        log(
          "Uploaded " +
            resp.sessions +
            " sessions. Analysis is running — open the report:",
        );
        const a = document.createElement("a");
        a.href = resp.reportUrl;
        a.target = "_blank";
        a.rel = "noopener";
        a.textContent = "▶ Open report (streams live, then shows the result)";
        a.style.cssText =
          "display:inline-block;margin-top:8px;color:#ffd9b0;font-weight:600";
        panel.appendChild(a);
      } else {
        log("Upload failed: " + (resp && resp.error));
      }
    } catch (e) {
      log("ERROR: " + (e && e.message ? e.message : String(e)));
    } finally {
      btn.disabled = false;
      gear.disabled = false;
      bar.style.opacity = "1";
    }
  }

  // Open the report tab synchronously on click (so the popup blocker allows it),
  // then start the scan. The report page streams live analysis status and
  // auto-advances to the finished report.
  btn.addEventListener("click", scan);

  // The background worker authorizes via the OAuth device-code flow on first
  // upload (or after a token expires). It opens the approval page in a new tab
  // with the code already embedded, so the user just clicks "Authorize"; the
  // code is shown only as a fallback in case the page doesn't prefill it.
  chrome.runtime.onMessage.addListener((msg) => {
    if (msg && msg.type === "authPrompt") {
      log(
        'Click "Authorize" in the opened tab, then it continues automatically.',
      );
      if (msg.userCode) log("  (if asked for a code: " + msg.userCode + ")");
    }
  });

  // Load saved config (history window + instance) written by the settings panel.
  chrome.storage.local.get("config", (d) => {
    if (d && d.config) cfg = Object.assign(cfg, d.config);
    updateLabel();
  });
})();
