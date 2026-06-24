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
  // Config (history window) comes from the options page; this is the default
  // until it loads from chrome.storage. windowDays scopes which conversations
  // are sent (0 = all time).
  let cfg = { windowDays: 7 };

  const btn = document.createElement("button");
  function updateLabel() {
    btn.textContent = cfg.windowDays > 0 ? "aiscan: scan last " + cfg.windowDays + "d" : "aiscan: scan all";
  }
  updateLabel();

  const gear = document.createElement("button");
  gear.textContent = "⚙";
  gear.title = "aiscan settings (history window)";
  gear.style.cssText =
    "position:fixed;z-index:2147483647;bottom:16px;right:" +
    "calc(16px + 150px);padding:8px 10px;background:#3a3a3f;color:#fff;border:none;" +
    "border-radius:8px;font:13px system-ui;cursor:pointer;box-shadow:0 2px 10px rgba(0,0,0,.35)";
  gear.addEventListener("click", () => chrome.runtime.sendMessage({ type: "options" }));
  document.documentElement.appendChild(gear);
  btn.style.cssText =
    "position:fixed;z-index:2147483647;bottom:16px;right:16px;padding:8px 12px;" +
    "background:#da7756;color:#fff;border:none;border-radius:8px;" +
    "font:13px system-ui,sans-serif;cursor:pointer;box-shadow:0 2px 10px rgba(0,0,0,.35)";
  document.documentElement.appendChild(btn);

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
      (o) => o && o.uuid && Array.isArray(o.capabilities) && o.capabilities.includes("chat")
    );
    if (chat.length > 1) {
      log("  " + chat.length + " chat orgs; using \"" + (chat[0].name || chat[0].uuid) + "\"");
    }
    if (chat[0]) return chat[0].uuid;
    throw new Error("no organization with the \"chat\" capability found");
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
    await Promise.all(Array.from({ length: Math.min(limit, items.length) }, worker));
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
      let list = await getJSON("/api/organizations/" + org + "/chat_conversations");
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
        log("  " + list.length + " of " + total + " active in last " + cfg.windowDays + " days");
      }
      if (!list.length) return;

      log("Fetching transcripts…");
      let done = 0;
      const conversations = await mapLimit(list, CONCURRENCY, async (c) => {
        const full = await getJSON(
          "/api/organizations/" + org + "/chat_conversations/" + c.uuid +
            "?tree=True&rendering_mode=messages&render_all_tools=true"
        );
        done++;
        if (done % 5 === 0 || done === list.length) log("  " + done + "/" + list.length);
        return full;
      });

      log("Uploading " + conversations.length + " conversations…");
      const resp = await chrome.runtime.sendMessage({
        type: "upload",
        conversations,
        windowDays: cfg.windowDays,
      });
      if (resp && resp.ok) {
        log("Uploaded " + resp.sessions + " sessions. Analysis is running — open the report:");
        const a = document.createElement("a");
        a.href = resp.reportUrl;
        a.target = "_blank";
        a.rel = "noopener";
        a.textContent = "▶ Open report (streams live, then shows the result)";
        a.style.cssText = "display:inline-block;margin-top:8px;color:#ffd9b0;font-weight:600";
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
      log("Authorize this extension in the opened tab, then it continues automatically.");
      if (msg.userCode) log("  confirmation code: " + msg.userCode);
    }
  });

  // Load saved config (history window) from the options page.
  chrome.storage.local.get("config", (d) => {
    if (d && d.config) cfg = Object.assign(cfg, d.config);
    updateLabel();
  });
})();
