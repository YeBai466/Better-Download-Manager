// Browser-integration background worker. Intercepts downloads, cancels the
// browser's own download and forwards the request to the local B Download
// Manager HTTP endpoint. Works in Chrome/Edge (MV3) and Firefox (the `api`
// shim picks `browser` when present).

const api = typeof browser !== "undefined" ? browser : chrome;

const DEFAULT_PORT = 9614;

// URLs we have deliberately re-issued to the browser (fallback when the app is
// not running). They must pass through without being intercepted again.
const bypass = new Set();

async function getConfig() {
  const { enabled = true, port = DEFAULT_PORT } = await api.storage.local.get([
    "enabled",
    "port",
  ]);
  return { enabled, port };
}

function endpoint(port) {
  return `http://127.0.0.1:${port}`;
}

function basename(p) {
  if (!p) return "";
  return p.split(/[\\/]/).pop();
}

async function collectCookies(url) {
  try {
    const cookies = await api.cookies.getAll({ url });
    return cookies.map((c) => `${c.name}=${c.value}`).join("; ");
  } catch {
    return "";
  }
}

api.downloads.onCreated.addListener(async (item) => {
  const url = item.finalUrl || item.url;
  if (!url || !/^https?:/i.test(url)) return;

  if (bypass.has(url)) {
    bypass.delete(url);
    return; // our own fallback download — let it proceed
  }

  const { enabled, port } = await getConfig();
  if (!enabled) return;

  // Cancel and remove the browser's download entry.
  try { await api.downloads.cancel(item.id); } catch {}
  try { await api.downloads.erase({ id: item.id }); } catch {}

  const payload = {
    url,
    filename: basename(item.filename) || "",
    referrer: item.referrer || "",
    mime: item.mime || "",
    fileSize: item.fileSize > 0 ? item.fileSize : item.totalBytes > 0 ? item.totalBytes : 0,
    userAgent: navigator.userAgent,
    cookie: await collectCookies(url),
    headers: {},
  };

  try {
    const res = await fetch(`${endpoint(port)}/download`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });
    if (!res.ok) throw new Error(`status ${res.status}`);
    setBadge("✓", "#2db84d");
  } catch (e) {
    // App not reachable: fall back to a normal browser download.
    setBadge("!", "#d93025");
    bypass.add(url);
    try { await api.downloads.download({ url }); } catch {}
  }
});

function setBadge(text, color) {
  const action = api.action || api.browserAction; // MV3 vs Firefox MV2
  if (!action) return;
  try {
    action.setBadgeText({ text });
    action.setBadgeBackgroundColor({ color });
    setTimeout(() => action.setBadgeText({ text: "" }), 2500);
  } catch {}
}
