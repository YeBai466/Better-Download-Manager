// Formatting helpers for sizes, speeds, durations and dates.

export function formatBytes(bytes: number): string {
  if (bytes < 0) return "未知";
  if (bytes === 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  const i = Math.floor(Math.log(bytes) / Math.log(1024));
  const value = bytes / Math.pow(1024, i);
  return `${value.toFixed(i === 0 ? 0 : 2)} ${units[i]}`;
}

export function formatSpeed(bytesPerSec: number): string {
  if (bytesPerSec <= 0) return "0 B/s";
  return `${formatBytes(bytesPerSec)}/s`;
}

export function formatETA(seconds: number): string {
  if (seconds < 0) return "--";
  if (seconds < 60) return `${Math.round(seconds)} 秒`;
  const m = Math.floor(seconds / 60);
  const s = Math.round(seconds % 60);
  if (m < 60) return `${m} 分 ${s} 秒`;
  const h = Math.floor(m / 60);
  return `${h} 时 ${m % 60} 分`;
}

export function formatDate(iso: string): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "";
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

export const statusLabels: Record<string, string> = {
  queued: "排队中",
  connecting: "连接中",
  downloading: "下载中",
  paused: "已暂停",
  completed: "已完成",
  error: "错误",
};
