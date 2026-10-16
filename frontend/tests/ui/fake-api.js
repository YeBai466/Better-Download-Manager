export const EVT_TASK_UPDATE = "task:update";
const listeners = new Set();
let task;
let adds = 0;
const delay = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
export function onEvent(name, callback) {
  if (name === EVT_TASK_UPDATE) listeners.add(callback);
  return () => listeners.delete(callback);
}
const settings = {
  language: "zh",
  connections: 8,
  proxy: { mode: "none", url: "", username: "", password: "" },
};
export const api = {
  async categories() {
    return ["General", "Documents"];
  },
  async getSettings() {
    return settings;
  },
  async consumePendingAdd() {
    return {};
  },
  async resolveSaveDir() {
    await delay(50);
    return "C:\\Downloads";
  },
  async probeURL({ url }) {
    await delay(url.includes("slow") ? 700 : 50);
    const name = url.includes("slow") ? "slow.bin" : "fast.bin";
    return {
      filename: name,
      category: "General",
      totalSize: 8192,
      resumable: true,
    };
  },
  async addURL(request) {
    adds++;
    document.getElementById("calls").textContent = `Add calls: ${adds}`;
    await delay(300);
    task = {
      id: "fast",
      revision: 2,
      filename: request.filename || "fast.bin",
      savePath: `${request.saveDir}\\${request.filename || "fast.bin"}`,
      status: "completed",
      error: "",
      downloaded: 8192,
      totalSize: 8192,
      resumable: true,
      connections: 1,
      progress: 1,
      speed: 0,
      etaSeconds: 0,
      segments: [],
    };
    for (const callback of listeners) callback(task);
    return { ...task, revision: 1, status: "connecting", progress: 0 };
  },
  async getTask() {
    return task;
  },
  async cancelHandoff() {},
  async chooseFolder() {
    return "C:\\Custom";
  },
  async pauseTask() {},
  async startTask() {},
  async removeTask() {},
  async openFile() {},
  async openFolder() {},
};
