// Pause first; cancel only after the application acknowledges durable receipt.
const api = typeof browser !== "undefined" ? browser : chrome;
const DEFAULT_PORT = 9614;
const PENDING_PREFIX = "handoff:";

async function getConfig() {
  const { enabled = true, port = DEFAULT_PORT } = await api.storage.local.get([
    "enabled",
    "port",
  ]);
  return { enabled, port };
}
const endpoint = (port) => `http://127.0.0.1:${port}`;
const basename = (name) => (name || "").split(/[\\/]/).pop();
async function request(url, options = {}) {
  const response = await fetch(url, {
    ...options,
    signal: AbortSignal.timeout(5000),
  });
  const body = await response.json().catch(() => ({}));
  return { response, body };
}
async function collectCookies(url) {
  try {
    return (await api.cookies.getAll({ url }))
      .map((c) => `${c.name}=${c.value}`)
      .join("; ");
  } catch {
    return "";
  }
}
function setBadge(text, color) {
  const action = api.action || api.browserAction;
  if (action) {
    Promise.resolve(action.setBadgeText({ text })).catch(() => {});
    Promise.resolve(action.setBadgeBackgroundColor({ color })).catch(() => {});
  }
}
async function finish(pending, accepted) {
  if (accepted) {
    // A failed cancel must retain the receipt so reconciliation can retry.
    await api.downloads.cancel(pending.browserId);
    await api.downloads.erase({ id: pending.browserId }).catch(() => {});
  } else {
    await api.downloads.resume(pending.browserId);
  }
  await api.storage.local.remove(PENDING_PREFIX + pending.requestId);
  setBadge(accepted ? "✓" : "", accepted ? "#2db84d" : "#888888");
}
async function reconcileOne(pending, explicitFailure = false) {
  try {
    const { response, body } = await request(
      `${endpoint(pending.port)}/download/status?requestId=${encodeURIComponent(pending.requestId)}`,
    );
    if (!response.ok) return;
    if (body.accepted) await finish(pending, true);
    else if (
      body.state === "failed" ||
      (explicitFailure && body.state === "missing")
    )
      await finish(pending, false);
  } catch {
    setBadge("!", "#d93025");
  }
}
async function reconcile() {
  const saved = await api.storage.local.get(null);
  await Promise.allSettled(
    Object.entries(saved)
      .filter(([key]) => key.startsWith(PENDING_PREFIX))
      .map(([, pending]) => reconcileOne(pending)),
  );
}
api.downloads.onCreated.addListener((item) => {
  intercept(item).catch(() => setBadge("!", "#d93025"));
});
async function intercept(item) {
  const url = item.finalUrl || item.url;
  if (!url || !/^https?:/i.test(url)) return;
  const { enabled, port } = await getConfig();
  if (!enabled) return;
  // If the application is absent, the original browser download keeps running.
  try {
    const { response, body } = await request(`${endpoint(port)}/ping`);
    if (
      !response.ok ||
      body.app !== "Better Download Manager" ||
      body.protocol !== "2"
    )
      return;
  } catch {
    return;
  }
  try {
    await api.downloads.pause(item.id);
  } catch {
    return;
  }
  const pending = { requestId: crypto.randomUUID(), browserId: item.id, port };
  try {
    await api.storage.local.set({
      [PENDING_PREFIX + pending.requestId]: pending,
    });
  } catch {
    await api.downloads.resume(item.id);
    return;
  }
  const payload = {
    requestId: pending.requestId,
    url,
    filename: basename(item.filename),
    referrer: item.referrer || "",
    mime: item.mime || "",
    fileSize: Math.max(0, item.fileSize || item.totalBytes || 0),
    userAgent: navigator.userAgent,
    cookie: await collectCookies(url),
    headers: {},
  };
  try {
    const { response, body } = await request(`${endpoint(port)}/download`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });
    if (response.ok && body.accepted && body.requestId === pending.requestId)
      await finish(pending, true);
    else await reconcileOne(pending, true);
  } catch {
    // The POST may have succeeded before its response was lost. Retain the
    // paused original until the receipt can be queried, including after restart.
    setBadge("!", "#d93025");
    await reconcileOne(pending);
  }
}
api.alarms.onAlarm.addListener((alarm) => {
  if (alarm.name === "bdm-reconcile") reconcile();
});
api.runtime.onStartup.addListener(reconcile);
api.runtime.onInstalled.addListener(reconcile);
api.runtime.onMessage.addListener((message, _sender, sendResponse) => {
  if (message?.type !== "reconcile") return;
  reconcile().then(() => sendResponse({ ok: true }));
  return true;
});
api.alarms.create("bdm-reconcile", { periodInMinutes: 1 });
reconcile();
