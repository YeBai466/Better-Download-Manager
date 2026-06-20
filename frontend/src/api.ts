// Thin wrappers over the generated Wails bindings and event runtime.
import { Events } from "@wailsio/runtime";
import { DownloadService } from "../bindings/github.com/yebai/better-download-manager/internal/service/index.js";
import type { AddRequest, ExtStatus, AddPrefill } from "../bindings/github.com/yebai/better-download-manager/internal/service/models.js";
import type { TaskInfo } from "../bindings/github.com/yebai/better-download-manager/internal/downloader/models.js";
import type { Settings } from "../bindings/github.com/yebai/better-download-manager/internal/config/models.js";

export type { TaskInfo, Settings, AddRequest, ExtStatus, AddPrefill };
type ProxySettings = Settings["proxy"];

export const EVT_TASK_UPDATE = "task:update";
export const EVT_TASK_REMOVED = "task:removed";
export const EVT_TAKEOVER = "takeover:request";

export const api = {
  listTasks: () => DownloadService.ListTasks(),
  addURL: (req: AddRequest) => DownloadService.AddURL(req),
  probeURL: (req: { url: string; headers?: Record<string, string>; proxy?: ProxySettings }) =>
    DownloadService.ProbeURL({ url: req.url, headers: req.headers ?? {}, proxy: req.proxy ?? ({ mode: "" } as ProxySettings) }),
  startTask: (id: string) => DownloadService.StartTask(id),
  pauseTask: (id: string) => DownloadService.PauseTask(id),
  removeTask: (id: string, deleteFile: boolean) => DownloadService.RemoveTask(id, deleteFile),
  startAll: () => DownloadService.StartAll(),
  pauseAll: () => DownloadService.PauseAll(),
  getSettings: () => DownloadService.GetSettings(),
  saveSettings: (cfg: Settings) => DownloadService.SaveSettings(cfg),
  categories: () => DownloadService.Categories(),
  chooseFolder: () => DownloadService.ChooseFolder(),
  openFile: (id: string) => DownloadService.OpenFile(id),
  openFolder: (id: string) => DownloadService.OpenFolder(id),
  installedBrowsers: () => DownloadService.InstalledBrowsers(),
  prepareManualInstall: () => DownloadService.PrepareManualInstall(),
  extStatus: () => DownloadService.BrowserExtensionStatus(),
  browserExtensionConfigured: () => DownloadService.BrowserExtensionConfigured(),
  installExt: (browsers: string[]) => DownloadService.InstallBrowserExtension(browsers),
  uninstallExt: (browsers: string[]) => DownloadService.UninstallBrowserExtension(browsers),
  resolveSaveDir: (category: string) => DownloadService.ResolveSaveDir(category),
  showAddWindow: (p: { url?: string; filename?: string; headers?: Record<string, string> }) =>
    DownloadService.ShowAddWindow({ url: p.url ?? "", filename: p.filename ?? "", headers: p.headers ?? {} }),
  consumePendingAdd: (window: string) => DownloadService.ConsumePendingAdd(window),
  checkForUpdates: () => DownloadService.CheckForUpdates(),
  downloadUpdate: (downloadUrl: string) => DownloadService.DownloadUpdate(downloadUrl),
  openURL: (url: string) => DownloadService.OpenURL(url),
};

// onEvent subscribes to a backend event and returns an unsubscribe function.
export function onEvent<T = any>(name: string, cb: (data: T) => void): () => void {
  return Events.On(name, (e: any) => cb(e.data as T));
}
