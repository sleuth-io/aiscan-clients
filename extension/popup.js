// aiscan claude.ai spike — popup (view only).
//
// The popup is just a trigger + a window onto the background worker's progress.
// Clicking the button messages the worker; the actual capture runs in
// background.js so it survives the popup closing. Progress is read from
// chrome.storage, so reopening the popup shows the latest state.

function render(status) {
  const log = document.getElementById("log");
  log.textContent = status && status.log ? status.log.join("\n") : "";
  log.scrollTop = log.scrollHeight;
  const btn = document.getElementById("scan");
  btn.disabled = !!(status && status.running);
  btn.textContent = status && status.running
    ? "Scanning…"
    : "Scan all my claude.ai conversations";
}

function poll() {
  chrome.storage.local.get("status", (cur) => render(cur.status));
}

document.getElementById("scan").addEventListener("click", () => {
  chrome.runtime.sendMessage({ type: "scan" });
  // optimistic: reflect running state immediately
  render({ running: true, log: ["Starting…"] });
});

setInterval(poll, 500);
poll();
