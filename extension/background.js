// Background worker — uploads the captured batch to the local aiscan daemon.
//
// The capture itself runs in the content script (content.js) on the claude.ai
// page, where fetch carries first-party cookies. This worker exists only to do
// the POST to http://127.0.0.1, which the https claude.ai page can't do
// directly (mixed content). Runs as a service worker in Chrome and an
// event-page script in Firefox.

const DAEMON = "http://127.0.0.1:8765/ingest";

chrome.runtime.onMessage.addListener((msg, _sender, sendResponse) => {
  if (msg && msg.type === "options") {
    chrome.runtime.openOptionsPage();
    return false;
  }
  if (msg && msg.type === "upload") {
    (async () => {
      try {
        const res = await fetch(DAEMON, {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({
            source: "claude-web",
            captured_at: new Date().toISOString(),
            role: msg.role || "",
            window_days: msg.windowDays || 0,
            conversations: msg.conversations || [],
          }),
        });
        const text = await res.text();
        if (!res.ok) sendResponse({ ok: false, error: "daemon " + res.status + ": " + text });
        else sendResponse({ ok: true, text });
      } catch (e) {
        sendResponse({ ok: false, error: (e && e.message ? e.message : String(e)) + " (is the daemon running? aiscan serve --local)" });
      }
    })();
    return true; // keep the message channel open for the async response
  }
  return false;
});
