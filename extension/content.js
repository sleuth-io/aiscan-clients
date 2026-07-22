// Content script — runs on claude.ai / chatgpt.com / gemini.google.com in the
// page's own origin.
//
// A declared content script auto-runs in both Chrome and Firefox without a
// separate host-permission grant, and its fetch() is same-origin to the site,
// so it carries your first-party session credentials (exactly like running
// fetch in the page console). It is headless: the extension's tab page (app.js)
// starts a sync, the background worker opens this site in a tab and sends
// "scan:start", and this script pulls the conversations raw and hands them to
// the background worker, which authorizes and uploads them to the configured
// Pulse instance (the cross-origin upload + OAuth can't run on the site's
// origin). Progress goes back as "scan:progress" messages and the run ends
// with a "scan:done"; the tab page renders both from the background's state.
// Parsing is the server's job — the client never transcodes.
//
// Layout: provider adapters (what to fetch, per site) → HTTP helpers → scan
// orchestration → message handling.

(function () {
  if (window.__aiscanInjected) return;
  window.__aiscanInjected = true;

  const CONCURRENCY = 5;

  // The sync run this scan belongs to. Minted by the background orchestrator;
  // echoed on every progress/done message so the orchestrator can drop
  // messages from a tab that belongs to a cancelled or superseded run.
  let currentSyncId = null;
  let scanning = false;

  // Progress line -> background worker. Fire-and-forget: a scan must not die
  // because a status line had nowhere to go (e.g. the worker is mid-restart).
  // These messages double as the MV3 keepalive — each one resets the worker's
  // idle timer while a long fetch loop runs.
  function report(line) {
    try {
      chrome.runtime
        .sendMessage({ type: "scan:progress", syncId: currentSyncId, line })
        .catch(() => {});
    } catch (_) {}
  }

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
        // Only the org you're actually signed into — see findActiveChatOrg.
        this.org = await findActiveChatOrg();
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

  function readCookie(name) {
    for (const part of document.cookie.split(";")) {
      const i = part.indexOf("=");
      if (i > 0 && part.slice(0, i).trim() === name)
        return decodeURIComponent(part.slice(i + 1));
    }
    return null;
  }

  // The one chat org you're currently signed into — never the others. An account
  // often has several (a personal org plus a team workspace), and we deliberately
  // scan only the active one: uploading conversations from an account you aren't
  // looking at would be a surprise, and surprise is the thing this client must
  // never do.
  //
  // claude.ai itself records the current org in the lastActiveOrg cookie as you
  // switch, so read that rather than guess. Orgs without the "chat" capability
  // (enterprise "api", individual "api_individual") 403 on the chat endpoints, so
  // they're never candidates — if the cookie names one, fall back to the first
  // chat org and say which we picked.
  async function findActiveChatOrg() {
    const orgs = await getJSON("/api/organizations");
    const list = Array.isArray(orgs) ? orgs : [];
    const chat = list.filter(
      (o) =>
        o &&
        o.uuid &&
        Array.isArray(o.capabilities) &&
        o.capabilities.includes("chat"),
    );
    if (!chat.length)
      throw new Error('no organization with the "chat" capability found');

    const active = readCookie("lastActiveOrg");
    const match = chat.find((o) => o.uuid === active);
    const picked = match || chat[0];
    // With more than one chat org, name the one we scanned: which account the
    // data came from is exactly the kind of thing you should never have to guess.
    if (chat.length > 1)
      report(
        "  " +
          chat.length +
          ' chat orgs; scanning "' +
          (picked.name || picked.uuid) +
          '"' +
          (match ? "" : " (no active org cookie)"),
      );
    return picked.uuid;
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

  async function scan({ force = false } = {}) {
    let synced = 0;
    try {
      report("Listing " + provider.label + " conversations…");
      const list = await provider.listConversations();
      report("  found " + list.length + " conversations");
      if (!list.length) return { ok: true, synced: 0 };

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

      report(
        force
          ? "Asking the server for everything again…"
          : "Asking the server what it still needs…",
      );
      const plan = await chrome.runtime.sendMessage({
        type: "plan",
        provider: provider.name,
        available,
        force,
      });
      if (!plan || !plan.ok)
        return { ok: false, synced: 0, error: (plan && plan.error) || "sync plan failed" };
      const needed = plan.neededSpans || [];
      if (!needed.length) {
        report("Already up to date — nothing to sync.");
        return { ok: true, synced: 0 };
      }
      report("  " + needed.length + " span(s) to sync");

      // Undated conversations can't be placed in a span; fold them into the most
      // recent needed span so they still get uploaded exactly once.
      const lastSpan = needed[needed.length - 1];
      for (const span of needed) {
        const s = Date.parse(span.start);
        const e = Date.parse(span.end);
        const inSpan = list.filter((c) => {
          const t = tsOf(c);
          return isNaN(t) ? span === lastSpan : t >= s && t < e;
        });

        let conversations = [];
        if (inSpan.length) {
          report(
            (force ? "Re-sending " : "Syncing ") +
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
              report("  " + done + "/" + inSpan.length);
            return full;
          });
        } else {
          // No conversations in this span — an empty upload records it as
          // scanned-and-empty so the server never asks for it again.
          report("Syncing " + fmtSpan(span) + " — empty");
        }

        const resp = await chrome.runtime.sendMessage({
          type: "upload",
          provider: provider.name,
          conversations,
          span,
          // Storing is idempotent on window + content, so on a re-send the server
          // needs telling to act on bytes it already has — otherwise it accepts the
          // upload and does nothing with it.
          force,
        });
        if (resp && resp.ok) synced += resp.sessions;
        else report("  upload failed: " + (resp && resp.error));
      }

      report("Synced " + synced + " sessions.");
      return { ok: true, synced };
    } catch (e) {
      const error = e && e.message ? e.message : String(e);
      report("ERROR: " + error);
      return { ok: false, synced, error };
    }
  }

  // ---- Message handling --------------------------------------------------
  // The background orchestrator drives everything: "ping" answers its
  // is-the-content-script-loaded poll after it opens this tab; "scan:start"
  // kicks off a scan whose terminal result goes back as one "scan:done"
  // (success, failure, or not-signed-in alike — the orchestrator marks the
  // site and moves on to the next one either way).
  chrome.runtime.onMessage.addListener((msg, _sender, sendResponse) => {
    if (!msg) return false;
    if (msg.type === "ping") {
      sendResponse({ ready: true, host: location.hostname });
      return false;
    }
    if (msg.type === "scan:start") {
      if (scanning) {
        sendResponse({ ok: false, error: "busy" });
        return false;
      }
      scanning = true;
      currentSyncId = msg.syncId;
      sendResponse({ ok: true }); // ack now; the result travels via scan:done
      scan({ force: !!msg.force })
        .then((result) => {
          chrome.runtime
            .sendMessage(
              Object.assign(
                { type: "scan:done", syncId: currentSyncId },
                result,
              ),
            )
            .catch(() => {});
        })
        .finally(() => {
          scanning = false;
        });
      return false;
    }
    return false;
  });

  // Written by the removed session-selection feature (shipped in v0.1.2);
  // clear it so stale exclusion lists don't linger in storage.
  chrome.storage.local.remove("excluded", () => void chrome.runtime.lastError);
})();
