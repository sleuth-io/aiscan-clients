// Loads/saves the aiscan extension config (role profile + history window) to
// chrome.storage. content.js reads this and ships it with each upload, so the
// daemon classifies personal work correctly and scans the matching window.

const roleEl = document.getElementById("role");
const windowEl = document.getElementById("windowDays");
const statusEl = document.getElementById("status");

chrome.storage.local.get("config", (d) => {
  const cfg = d.config || {};
  if (cfg.role) roleEl.value = cfg.role;
  if (cfg.windowDays != null) windowEl.value = cfg.windowDays;
});

document.getElementById("save").addEventListener("click", () => {
  const config = {
    role: roleEl.value.trim(),
    windowDays: Math.max(0, parseInt(windowEl.value, 10) || 0),
  };
  chrome.storage.local.set({ config }, () => {
    statusEl.textContent = "Saved.";
    setTimeout(() => (statusEl.textContent = ""), 1500);
  });
});
