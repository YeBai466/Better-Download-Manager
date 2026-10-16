const api = typeof browser !== "undefined" ? browser : chrome;
const DEFAULT_PORT = 9614;

const enabledEl = document.getElementById("enabled");
const portEl = document.getElementById("port");
const dotEl = document.getElementById("dot");
const statusEl = document.getElementById("statusText");

async function load() {
  const { enabled = true, port = DEFAULT_PORT } = await api.storage.local.get([
    "enabled",
    "port",
  ]);
  enabledEl.checked = enabled;
  portEl.value = port;
  ping(port);
}

async function ping(port) {
  try {
    const res = await fetch(`http://127.0.0.1:${port}/ping`, {
      cache: "no-store",
      signal: AbortSignal.timeout(2000),
    });
    const data = await res.json();
    dotEl.className = "dot on";
    statusEl.textContent = `已连接 · v${data.version}`;
  } catch {
    dotEl.className = "dot off";
    statusEl.textContent = "未运行（请启动应用）";
  }
}

enabledEl.addEventListener("change", () => {
  api.storage.local.set({ enabled: enabledEl.checked });
});

portEl.addEventListener("change", () => {
  const input = Number(portEl.value);
  const port =
    Number.isInteger(input) && input >= 1 && input <= 65535
      ? input
      : DEFAULT_PORT;
  portEl.value = port;
  api.storage.local.set({ port });
  ping(port);
});

load();

api.runtime.sendMessage({ type: "reconcile" }).catch(() => {});
api.storage.local.get(null).then((saved) => {
  const count = Object.keys(saved).filter((key) =>
    key.startsWith("handoff:"),
  ).length;
  if (count)
    statusEl.textContent = `${count} 个下载正在确认接管；原下载已暂停，连接应用后自动重试`;
});
