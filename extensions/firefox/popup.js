const api = typeof browser !== "undefined" ? browser : chrome;
const DEFAULT_PORT = 9614;

const enabledEl = document.getElementById("enabled");
const portEl = document.getElementById("port");
const dotEl = document.getElementById("dot");
const statusEl = document.getElementById("statusText");

async function load() {
  const { enabled = true, port = DEFAULT_PORT } = await api.storage.local.get(["enabled", "port"]);
  enabledEl.checked = enabled;
  portEl.value = port;
  ping(port);
}

async function ping(port) {
  try {
    const res = await fetch(`http://127.0.0.1:${port}/ping`, { cache: "no-store" });
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
  const port = Number(portEl.value) || DEFAULT_PORT;
  api.storage.local.set({ port });
  ping(port);
});

load();
