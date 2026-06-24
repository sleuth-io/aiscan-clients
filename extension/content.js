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

  const btn = document.createElement("button");
  btn.textContent = "Scan and upload my chats";
  // The window scope no longer fits the label (the main button now reads as a
  // plain action), so surface it in the tooltip instead — refreshed after Save.
  function updateLabel() {
    btn.title =
      cfg.windowDays > 0
        ? "Scans your claude.ai sessions from the last " +
          cfg.windowDays +
          " days"
        : "Scans all your claude.ai sessions";
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

  settings.appendChild(mkLabel("Pulse instance"));
  settings.appendChild(
    mkHint(
      "Where uploads go. Use http://dev.pulse.sleuth.io for local dev, or https://app.skills.new for production. Just the base URL.",
    ),
  );
  const instanceEl = document.createElement("input");
  instanceEl.type = "text";
  instanceEl.placeholder = DEFAULT_INSTANCE;
  instanceEl.style.cssText =
    fieldCss + ";width:100%;font:12px ui-monospace,Menlo,monospace";
  settings.appendChild(instanceEl);

  settings.appendChild(mkLabel("Days of history to include"));
  const windowEl = document.createElement("input");
  windowEl.type = "number";
  windowEl.min = "0";
  windowEl.style.cssText = fieldCss + ";width:80px;font:13px system-ui";
  settings.appendChild(windowEl);
  settings.appendChild(
    mkHint(
      "Applies to claude.ai web sessions and your local Claude Code sessions. 0 = all time.",
    ),
  );

  const saveBtn = document.createElement("button");
  saveBtn.textContent = "Save";
  saveBtn.style.cssText =
    "padding:7px 14px;background:#236a91;color:#fff;border:none;border-radius:6px;cursor:pointer;font:13px system-ui";
  const signoutBtn = document.createElement("button");
  signoutBtn.textContent = "Sign out";
  signoutBtn.style.cssText =
    "margin-left:8px;padding:7px 14px;background:#7a3b3b;color:#fff;border:none;border-radius:6px;cursor:pointer;font:13px system-ui";
  const savedNote = document.createElement("span");
  savedNote.style.cssText = "margin-left:10px;color:#7fd18a;font-size:12px";
  settings.appendChild(saveBtn);
  settings.appendChild(signoutBtn);
  settings.appendChild(savedNote);
  settings.appendChild(
    mkHint(
      "Authorization happens automatically on your first scan (an approval tab opens). Sign out clears the cached token — use it after switching instances.",
    ),
  );
  document.documentElement.appendChild(settings);

  const flash = (msg) => {
    savedNote.textContent = msg;
    setTimeout(() => (savedNote.textContent = ""), 1500);
  };
  gear.addEventListener("click", () => {
    const showing = settings.style.display !== "none";
    if (!showing) {
      instanceEl.value = cfg.instanceUrl || DEFAULT_INSTANCE;
      windowEl.value = cfg.windowDays != null ? cfg.windowDays : 7;
      savedNote.textContent = "";
    }
    settings.style.display = showing ? "none" : "block";
  });
  saveBtn.addEventListener("click", () => {
    cfg.instanceUrl = (instanceEl.value.trim() || DEFAULT_INSTANCE).replace(
      /\/+$/,
      "",
    );
    cfg.windowDays = Math.max(0, parseInt(windowEl.value, 10) || 0);
    chrome.storage.local.set({ config: cfg }, () => {
      updateLabel();
      flash("Saved.");
    });
  });
  signoutBtn.addEventListener("click", () => {
    chrome.storage.local.remove("auth", () => flash("Signed out."));
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

  async function getJSON(url) {
    const r = await fetch(url, {
      credentials: "include",
      headers: { accept: "application/json" },
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
    btn.disabled = true;
    panel.textContent = "";
    try {
      log("Finding organization…");
      const org = await findOrg();
      log("  org = " + org);

      log("Listing conversations…");
      let list = await getJSON(
        "/api/organizations/" + org + "/chat_conversations",
      );
      const total = list.length;
      log("  found " + total + " conversations");
      if (!total) return;
      // Keep only conversations active within the window (by last update).
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
      const conversations = await mapLimit(list, CONCURRENCY, async (c) => {
        const full = await getJSON(
          "/api/organizations/" +
            org +
            "/chat_conversations/" +
            c.uuid +
            "?tree=True&rendering_mode=messages&render_all_tools=true",
        );
        done++;
        if (done % 5 === 0 || done === list.length)
          log("  " + done + "/" + list.length);
        return full;
      });

      log("Uploading " + conversations.length + " conversations…");
      const resp = await chrome.runtime.sendMessage({
        type: "upload",
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
    }
  }

  // Open the report tab synchronously on click (so the popup blocker allows it),
  // then start the scan. The report page streams live analysis status and
  // auto-advances to the finished report.
  btn.addEventListener("click", scan);

  // The background worker authorizes via the OAuth device-code flow on first
  // upload (or after a token expires). It opens the approval page in a new tab
  // and messages us the user code to confirm.
  chrome.runtime.onMessage.addListener((msg) => {
    if (msg && msg.type === "authPrompt") {
      log(
        "Authorize this extension in the opened tab, then it continues automatically.",
      );
      if (msg.userCode) log("  confirmation code: " + msg.userCode);
    }
  });

  // Load saved config (history window + instance) written by the settings panel.
  chrome.storage.local.get("config", (d) => {
    if (d && d.config) cfg = Object.assign(cfg, d.config);
    updateLabel();
  });
})();
