// Loads/saves the aiscan extension config (instance URL + history window) to
// chrome.storage. content.js reads the window and ships it with each upload;
// background.js reads instanceUrl to know where to authorize and upload.

const DEFAULT_INSTANCE = "http://dev.pulse.sleuth.io";

const instanceEl = document.getElementById("instanceUrl");
const windowEl = document.getElementById("windowDays");
const statusEl = document.getElementById("status");

chrome.storage.local.get("config", (d) => {
  const cfg = d.config || {};
  instanceEl.value = cfg.instanceUrl || DEFAULT_INSTANCE;
  if (cfg.windowDays != null) windowEl.value = cfg.windowDays;
});

function flash(msg) {
  statusEl.textContent = msg;
  setTimeout(() => (statusEl.textContent = ""), 1500);
}

document.getElementById("save").addEventListener("click", () => {
  const config = {
    instanceUrl: (instanceEl.value.trim() || DEFAULT_INSTANCE).replace(/\/+$/, ""),
    windowDays: Math.max(0, parseInt(windowEl.value, 10) || 0),
  };
  chrome.storage.local.set({ config }, () => flash("Saved."));
});

document.getElementById("signout").addEventListener("click", () => {
  chrome.storage.local.remove("auth", () => flash("Signed out."));
});
