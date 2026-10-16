// Formatting helpers for sizes, speeds, durations and dates.
import { t } from "./i18n";

export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return t("common.unknown");
  if (bytes === 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  const i = Math.min(
    units.length - 1,
    Math.max(0, Math.floor(Math.log(bytes) / Math.log(1024))),
  );
  const value = bytes / Math.pow(1024, i);
  return `${value.toFixed(i === 0 ? 0 : 2)} ${units[i]}`;
}

export function formatSpeed(bytesPerSec: number): string {
  if (bytesPerSec <= 0) return "0 B/s";
  return `${formatBytes(bytesPerSec)}/s`;
}

export function formatETA(seconds: number): string {
  if (seconds < 0) return "--";
  if (seconds < 60) return t("fmt.sec", { n: Math.round(seconds) });
  const m = Math.floor(seconds / 60);
  const s = Math.round(seconds % 60);
  if (m < 60) return t("fmt.minSec", { m, s });
  const h = Math.floor(m / 60);
  return t("fmt.hourMin", { h, m: m % 60 });
}

export function formatDate(iso: string): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "";
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

// statusLabel translates a task status; reads the active language at call time.
export function statusLabel(status: string): string {
  const key = `st.${status}`;
  const label = t(key);
  return label === key ? status : label;
}
