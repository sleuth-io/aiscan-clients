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
  // Config (Pulse instance) is edited in the inline settings panel below; this
  // is the default until it loads from chrome.storage. instanceUrl is read by
  // background.js to know where to authorize and upload.
  let cfg = { instanceUrl: DEFAULT_INSTANCE };

  // ---- Provider adapters -------------------------------------------------
  // claude.ai, chatgpt.com, and gemini.google.com expose different chat APIs,
  // but the scan flow is identical: list conversations -> ask the server which
  // spans it still needs -> fetch each needed transcript raw -> hand the batch
  // to the background worker. Each adapter knows only how to list and fetch on
  // its own origin
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
          body:
            "f.req=" +
            encodeURIComponent(freq) +
            "&at=" +
            encodeURIComponent(at),
        });
        if (!r.ok)
          throw new Error(
            r.status + " " + r.statusText + " — batchexecute " + rpcid,
          );
        // Responses are framed as )]}' then length-delimited JSON rows; ours is
        // the "wrb.fr" row whose second field is our rpcid.
        for (const line of (await r.text()).split("\n")) {
          if (!line.startsWith("[[")) continue;
          try {
            for (const row of JSON.parse(line))
              if (row[0] === "wrb.fr" && row[1] === rpcid)
                return JSON.parse(row[2]);
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
          return {
            id: c[0],
            title: c[1] || "",
            updated_at: ts,
            created_at: ts,
          };
        });
      },
      async fetchFull(item) {
        // hNvQHb returns the conversation's turns; upload the raw inner payload
        // and let the server parser walk the nested-array structure.
        const payload = await this.rpc("hNvQHb", [
          item.id,
          50,
          null,
          1,
          [0],
          [4],
          null,
          1,
        ]);
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
    title:
      "Syncs your " +
      provider.label +
      " chats to aiscan — uploads only what the server still needs",
    style:
      "padding:8px 14px;background:#da7756;color:#fff;border:none;cursor:pointer;" +
      "font:13px system-ui,sans-serif;border-right:1px solid rgba(0,0,0,.18)",
  });

  const gear = el("button", {
    text: "⚙",
    title: "Sleuth AI Insights settings (instance)",
    style:
      "padding:8px 11px 8px 9px;background:#c4634a;color:#fff;border:none;cursor:pointer;" +
      "font:18px system-ui;line-height:1;display:flex;align-items:center",
  });

  // Inline settings popover — anchored above the gear so the user never leaves
  // the page to edit the instance URL or to sign out. Reads/writes the same
  // chrome.storage.local "config" that loads below and that background.js reads
  // on upload; "Sign out" clears the cached OAuth token.
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

  // Pulse instance. Commits on blur/Enter, not per keystroke.
  const instanceHeader = mkLabel("Pulse instance");
  const instanceHint = mkHint(
    "Where uploads go. e.g. http://dev.pulse.sleuth.io for local dev, https://app.skills.new for production.",
  );
  const instanceEl = el("input", {
    type: "text",
    placeholder: DEFAULT_INSTANCE,
    style: fieldCss + ";width:100%;font:12px ui-monospace,Menlo,monospace",
  });

  // Account — Sign out.
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

  // Compose the panel in display order, then mount it once.
  [
    instanceHeader,
    instanceHint,
    instanceEl,
    signoutBtn,
    savedNote,
    signoutHint,
  ].forEach((n) => settings.appendChild(n));
  document.documentElement.appendChild(settings);

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

  gear.addEventListener("click", () => {
    const showing = settings.style.display !== "none";
    if (!showing) {
      // Opening settings clears any previous scan log out of the popover.
      panel.style.display = "none";
      panel.textContent = "";
      instanceEl.value = cfg.instanceUrl || DEFAULT_INSTANCE;
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
      headers: Object.assign(
        { accept: "application/json" },
        extraHeaders || {},
      ),
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
  // One conversation's timestamp (last activity, falling back to creation) in
  // ms, or NaN when the adapter couldn't date it.
  const tsOf = (c) => Date.parse(c.updated_at || c.created_at || "");
  // Render a span as "YYYY-MM-DD … YYYY-MM-DD" for the log lines.
  const day = (iso) => new Date(iso).toISOString().slice(0, 10);
  const fmtSpan = (s) => day(s.start) + " … " + day(s.end);

  // Append the "Open reports" link once a sync finishes. Reports live at the
  // instance's /aiscan index — each sync only deposits evidence; the server
  // builds the report separately.
  function showReportsLink(url) {
    panel.appendChild(
      el("a", {
        href: url,
        target: "_blank",
        rel: "noopener",
        text: "Open reports",
        style:
          "display:inline-block;margin-top:8px;color:#ffd9b0;font-weight:600",
      }),
    );
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
      const list = await provider.listConversations();
      log("  found " + list.length + " conversations");
      if (!list.length) return;

      // Offer the server our whole history (earliest activity … now); it replies
      // with just the spans it still needs, so we never re-fetch or re-upload
      // transcripts it already has. Undatable conversations don't move the floor.
      const times = list.map(tsOf).filter((t) => !isNaN(t));
      const earliest = times.reduce((a, b) => Math.min(a, b), Date.now());
      const available = [
        {
          start: new Date(times.length ? earliest : 0).toISOString(),
          end: new Date().toISOString(),
        },
      ];

      log("Asking the server what it still needs…");
      const plan = await chrome.runtime.sendMessage({
        type: "plan",
        provider: provider.name,
        available,
      });
      if (!plan || !plan.ok) {
        log("Sync plan failed: " + (plan && plan.error));
        return;
      }
      const needed = plan.neededSpans || [];
      if (!needed.length) {
        log("Already up to date — nothing to sync.");
        showReportsLink(plan.reportsUrl);
        return;
      }
      log("  " + needed.length + " span(s) to sync");

      // Undated conversations can't be placed in a span; fold them into the most
      // recent needed span so they still get uploaded exactly once.
      const lastSpan = needed[needed.length - 1];
      let synced = 0;
      for (const span of needed) {
        const s = Date.parse(span.start);
        const e = Date.parse(span.end);
        const inSpan = list.filter((c) => {
          const t = tsOf(c);
          return isNaN(t) ? span === lastSpan : t >= s && t < e;
        });

        let conversations = [];
        if (inSpan.length) {
          log(
            "Syncing " +
              fmtSpan(span) +
              " — " +
              inSpan.length +
              " conversations…",
          );
          let done = 0;
          conversations = await mapLimit(inSpan, CONCURRENCY, async (item) => {
            const full = await provider.fetchFull(item);
            done++;
            if (done % 5 === 0 || done === inSpan.length)
              log("  " + done + "/" + inSpan.length);
            return full;
          });
        } else {
          // No conversations in this span — an empty upload records it as
          // scanned-and-empty so the server never asks for it again.
          log("Syncing " + fmtSpan(span) + " — empty");
        }

        const resp = await chrome.runtime.sendMessage({
          type: "upload",
          provider: provider.name,
          conversations,
          span,
        });
        if (resp && resp.ok) synced += resp.sessions;
        else log("  upload failed: " + (resp && resp.error));
      }

      log("Synced " + synced + " sessions. Open the reports page:");
      showReportsLink(plan.reportsUrl);
    } catch (e) {
      log("ERROR: " + (e && e.message ? e.message : String(e)));
    } finally {
      btn.disabled = false;
      gear.disabled = false;
      bar.style.opacity = "1";
    }
  }

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

  // Load saved config (instance) written by the settings panel.
  chrome.storage.local.get("config", (d) => {
    if (d && d.config) cfg = Object.assign(cfg, d.config);
  });
})();
