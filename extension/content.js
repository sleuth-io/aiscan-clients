// Content script — runs on claude.ai / chatgpt.com / gemini.google.com in the
// page's own origin.
//
// A declared content script auto-runs in both Chrome and Firefox without a
// separate host-permission grant, and its fetch() is same-origin to the site,
// so it carries your first-party session credentials (exactly like running
// fetch in the page console). It injects a small button + status panel; on
// click it pulls the conversations raw and hands them to the background worker,
// which authorizes and uploads them to the configured Pulse instance (the
// cross-origin upload + OAuth can't run on the site's origin). Parsing is the
// server's job — the client never transcodes.
//
// Layout: provider adapters (what to fetch, per site) → DOM helper → UI (the
// button + settings/log popovers) → HTTP helpers → scan orchestration.

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
  // claude.ai, chatgpt.com, and gemini.google.com expose different chat APIs,
  // but the scan flow is identical: list conversations -> filter by the history
  // window -> fetch each transcript raw -> hand the batch to the background
  // worker. Each adapter knows only how to list and fetch on its own origin
  // (where the page's cookies / token apply). The adapter's `name` is sent to
  // background.js so it files the upload under the right provider dir; the
  // server picks the parser from there. Detection is purely by hostname.
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
          // A short page is the last one — this terminates without trusting
          // `total`, so a missing/overstated count can't spin the loop. The
          // `total` check just stops one request earlier on an exact boundary.
          if (items.length < limit || offset >= ((j && j.total) || all.length))
            break;
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
    "gemini.google.com": {
      name: "gemini",
      label: "Gemini",
      // Gemini's web backend speaks Google's `batchexecute` RPC (no REST). Calls
      // need an XSRF token (`at`) plus the build label / session id. The isolated
      // content world can't read window.WIZ_global_data, so we regex those out of
      // the app HTML (same-origin fetch, cookies attached). We only unwrap the
      // generic RPC transport here; the nested-array conversation format itself
      // is parsed server-side (uploaded as source=gemini-web).
      async tokens() {
        if (!this._tokens) {
          const html = await (
            await fetch(location.href, { credentials: "include" })
          ).text();
          const grab = (re) => {
            const m = html.match(re);
            return m ? m[1] : null;
          };
          const at = grab(/"SNlM0e":"([^"]+)"/);
          if (!at) throw new Error("not signed in to gemini.google.com");
          this._tokens = {
            at,
            bl: grab(/"cfb2h":"([^"]+)"/) || "",
            sid: grab(/"FdrFJe":"([^"]+)"/) || "",
          };
        }
        return this._tokens;
      },
      // One batchexecute RPC -> the inner payload for `rpcid` (transport envelope
      // stripped), or null. `inner` is the RPC-specific argument array.
      async rpc(rpcid, inner) {
        const { at, bl, sid } = await this.tokens();
        const url =
          "/_/BardChatUi/data/batchexecute?rpcids=" +
          rpcid +
          "&source-path=%2Fapp&bl=" +
          encodeURIComponent(bl) +
          "&f.sid=" +
          encodeURIComponent(sid) +
          "&hl=en&_reqid=" +
          Math.floor(Math.random() * 1e6) +
          "&rt=c";
        const freq = JSON.stringify([
          [[rpcid, JSON.stringify(inner), null, "generic"]],
        ]);
        const r = await fetch(url, {
          method: "POST",
          credentials: "include",
          headers: {
            "content-type": "application/x-www-form-urlencoded;charset=UTF-8",
          },
          body: "f.req=" + encodeURIComponent(freq) + "&at=" + encodeURIComponent(at),
        });
        if (!r.ok)
          throw new Error(r.status + " " + r.statusText + " — batchexecute " + rpcid);
        // Responses are framed as )]}' then length-delimited JSON rows; ours is
        // the "wrb.fr" row whose second field is our rpcid.
        for (const line of (await r.text()).split("\n")) {
          if (!line.startsWith("[[")) continue;
          try {
            for (const row of JSON.parse(line))
              if (row[0] === "wrb.fr" && row[1] === rpcid) return JSON.parse(row[2]);
          } catch (_) {}
        }
        return null;
      },
      async listConversations() {
        // MaZiqc is token-paged: the response's [1] is the continuation token
        // (null when there are no more), passed back as the request's 2nd arg.
        // The real terminator is token exhaustion; we also stop if the token
        // stops advancing (defensive) and cap the page count. id-dedup guards
        // against any overlap. Each entry is [id, title, …, [epochSec,nanos], …].
        const PAGE = 50;
        const convs = [];
        const seen = new Set();
        let token = null;
        for (let page = 0; page < 100; page++) {
          const resp = await this.rpc("MaZiqc", [PAGE, token, [0, null, 1]]);
          for (const c of (resp && resp[2]) || []) {
            if (c && c[0] && !seen.has(c[0])) {
              seen.add(c[0]);
              convs.push(c);
            }
          }
          const next = resp && resp[1];
          if (!next || next === token) break;
          token = next;
        }
        return convs.map((c) => {
          const ts = Array.isArray(c[5])
            ? new Date(c[5][0] * 1000).toISOString()
            : undefined;
          return { id: c[0], title: c[1] || "", updated_at: ts, created_at: ts };
        });
      },
      async fetchFull(item) {
        // hNvQHb returns the conversation's turns; upload the raw inner payload
        // and let the server parser walk the nested-array structure.
        const payload = await this.rpc(
          "hNvQHb",
          [item.id, 50, null, 1, [0], [4], null, 1],
        );
        return {
          conversation_id: item.id,
          title: item.title,
          updated_at: item.updated_at,
          payload,
        };
      },
    },
  };
  const provider = PROVIDERS[location.hostname] || PROVIDERS["claude.ai"];

  // ---- Tiny DOM helper ---------------------------------------------------
  // claude.ai enforces a Trusted-Types CSP that blocks string-to-HTML, so the
  // whole UI is built from real nodes. `el` keeps that declarative: props map
  // onto element properties (`style` -> cssText, `text` -> textContent, `on` ->
  // an {event: handler} map, anything else set directly); children are nodes or
  // strings (strings become text nodes).
  function el(tag, props, children) {
    const node = document.createElement(tag);
    for (const [k, v] of Object.entries(props || {})) {
      if (v == null) continue;
      if (k === "style") node.style.cssText = v;
      else if (k === "text") node.textContent = v;
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

  // ---- UI: a split button (scan | ⚙) with settings + log popovers --------
  const btn = el("button", {
    text: "Scan and upload my chats",
    style:
      "padding:8px 14px;background:#da7756;color:#fff;border:none;cursor:pointer;" +
      "font:13px system-ui,sans-serif;border-right:1px solid rgba(0,0,0,.18)",
  });
  // The window scope no longer fits the label (the button reads as a plain
  // action), so surface it in the tooltip instead — refreshed after each change.
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

  const gear = el("button", {
    text: "⚙",
    title: "Sleuth AI Insights settings (instance + history window)",
    style:
      "padding:8px 11px 8px 9px;background:#c4634a;color:#fff;border:none;cursor:pointer;" +
      "font:18px system-ui;line-height:1;display:flex;align-items:center",
  });

  // Inline settings popover — anchored above the gear so the user never leaves
  // the page to edit the instance URL / history window or to sign out. Reads/
  // writes the same chrome.storage.local "config" that loads below and that
  // background.js reads on upload; "Sign out" clears the cached OAuth token.
  const settings = el("div", {
    style:
      "position:fixed;z-index:2147483647;bottom:56px;right:16px;width:340px;display:none;" +
      "box-sizing:border-box;padding:12px;background:#1d1d1f;color:#eaeaea;border-radius:8px;" +
      "font:13px system-ui,-apple-system,sans-serif;box-shadow:0 2px 10px rgba(0,0,0,.35)",
  });

  const mkLabel = (text) =>
    el("div", { style: "font-weight:600;margin:0 0 4px", text });
  const mkHint = (text) =>
    el("div", {
      style: "color:#9a9aa0;font-size:11px;margin:2px 0 10px",
      text,
    });
  const fieldCss =
    "box-sizing:border-box;padding:5px 7px;border-radius:6px;border:1px solid #3a3a3f;" +
    "background:#111113;color:#eaeaea";

  // Persist immediately on every change — there is no Save button.
  const persist = () => chrome.storage.local.set({ config: cfg });

  // History window — a slider over discrete stops; the last stop (0) is all time.
  const WINDOW_STEPS = [7, 14, 30, 60, 90, 180, 0];
  const WINDOW_TICKS = ["7d", "14d", "30d", "60d", "90d", "180d", "All"];
  const windowText = (d) => (d > 0 ? "Last " + d + " days" : "All time");

  const windowValue = el("div", {
    style: "color:#da7756;font-weight:600;margin:-2px 0 6px",
  });
  const slider = el("input", {
    type: "range",
    min: "0",
    max: String(WINDOW_STEPS.length - 1),
    step: "1",
    style: "width:100%;accent-color:#da7756;cursor:pointer;margin:0",
  });
  const ticks = el(
    "div",
    {
      style:
        "display:flex;justify-content:space-between;color:#9a9aa0;font-size:10px;margin-top:2px",
    },
    WINDOW_TICKS.map((t) => el("span", { text: t })),
  );
  const windowHint = mkHint(
    "How far back to scan your " + provider.label + " sessions.",
  );

  // Pulse instance — dev only. Commits on blur/Enter, not per keystroke.
  const instanceHeader = mkLabel("Pulse instance");
  const instanceHint = mkHint(
    "Where uploads go. e.g. http://dev.pulse.sleuth.io for local dev, https://app.skills.new for production.",
  );
  const instanceEl = el("input", {
    type: "text",
    placeholder: DEFAULT_INSTANCE,
    style: fieldCss + ";width:100%;font:12px ui-monospace,Menlo,monospace",
  });

  // Account — Sign out lives behind dev mode (most users never need it).
  const signoutBtn = el("button", {
    text: "Sign out",
    style:
      "margin-top:8px;padding:7px 14px;background:#7a3b3b;color:#fff;border:none;" +
      "border-radius:6px;cursor:pointer;font:13px system-ui",
  });
  const savedNote = el("span", {
    style: "margin-left:10px;color:#7fd18a;font-size:12px",
  });
  const signoutHint = mkHint(
    "Authorization happens automatically on your first scan (an approval tab opens). Sign out clears the cached token — use it after switching instances.",
  );

  // Subtle dev-mode toggle: reveals the instance field + all help text.
  const devCheck = el("input", {
    type: "checkbox",
    style: "margin:0;accent-color:#6a6a70;cursor:pointer",
  });
  const devToggle = el(
    "label",
    {
      style:
        "display:flex;align-items:center;justify-content:flex-end;gap:6px;margin-top:12px;" +
        "padding-top:10px;border-top:1px solid #2c2c30;color:#6a6a70;font-size:11px;cursor:pointer",
    },
    [devCheck, el("span", { text: "Developer mode" })],
  );

  // Compose the panel in display order, then mount it once.
  [
    mkLabel("History window"),
    windowValue,
    slider,
    ticks,
    windowHint,
    instanceHeader,
    instanceHint,
    instanceEl,
    signoutBtn,
    savedNote,
    signoutHint,
    devToggle,
  ].forEach((n) => settings.appendChild(n));
  document.documentElement.appendChild(settings);

  slider.addEventListener("input", () => {
    cfg.windowDays = WINDOW_STEPS[parseInt(slider.value, 10)] || 0;
    windowValue.textContent = windowText(cfg.windowDays);
    updateLabel();
    persist();
  });

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

  const flash = (msg) => {
    savedNote.textContent = msg;
    setTimeout(() => (savedNote.textContent = ""), 1500);
  };
  signoutBtn.addEventListener("click", () => {
    chrome.storage.local.remove("auth", () => flash("Signed out."));
  });

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
    devOnly.forEach((node) => (node.style.display = on ? "" : "none"));
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

  // Split button: the wide main part runs the scan; the narrow ⚙ toggles the
  // settings popover. Both live in one rounded bar so it reads as one control.
  const bar = el(
    "div",
    {
      style:
        "position:fixed;z-index:2147483647;bottom:16px;right:16px;display:inline-flex;" +
        "border-radius:8px;overflow:hidden;box-shadow:0 2px 10px rgba(0,0,0,.35)",
    },
    [btn, gear],
  );
  document.documentElement.appendChild(bar);

  // Status log popover — shares the gear's anchor; scan() writes progress here.
  const panel = el("div", {
    style:
      "position:fixed;z-index:2147483647;bottom:56px;right:16px;width:340px;max-height:260px;" +
      "overflow:auto;display:none;padding:8px;background:#1d1d1f;color:#eaeaea;border-radius:8px;" +
      "font:11px ui-monospace,Menlo,monospace;white-space:pre-wrap;box-shadow:0 2px 10px rgba(0,0,0,.35)",
  });
  document.documentElement.appendChild(panel);
  // Append as a text node (not textContent +=) so DOM children like the report
  // link aren't wiped on the next log line.
  const log = (m) => {
    panel.style.display = "block";
    panel.appendChild(document.createTextNode(m + "\n"));
    panel.scrollTop = panel.scrollHeight;
  };

  // ---- HTTP helpers ------------------------------------------------------
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

  // ---- Scan orchestration ------------------------------------------------
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
      // Keep only conversations active within the window (by last update). Every
      // adapter normalizes its list timestamps to ISO strings here.
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
        panel.appendChild(
          el("a", {
            href: resp.reportUrl,
            target: "_blank",
            rel: "noopener",
            text: "▶ Open report (streams live, then shows the result)",
            style:
              "display:inline-block;margin-top:8px;color:#ffd9b0;font-weight:600",
          }),
        );
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
