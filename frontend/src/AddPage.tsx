import { useCallback, useEffect, useState } from "react";
import { Window } from "@wailsio/runtime";
import "./styles.css";
import { api, onEvent, EVT_TASK_UPDATE, type Settings, type TaskInfo } from "./api";
import { formatBytes, formatSpeed, formatETA, statusLabel } from "./format";
import { categoryLabel } from "./components/Sidebar";
import { t as tr, setLang, useLang } from "./i18n";

type Mode = "form" | "progress";
type ProxySettings = Settings["proxy"];

const defaultProxy = { mode: "system", url: "", username: "", password: "" } as ProxySettings;

// Each add-download window is opened with a unique name in the URL (?w=add-N);
// the prefill is keyed by that name so concurrent windows don't read each
// other's data.
const winName = new URLSearchParams(window.location.search).get("w") ?? "";

function cleanHeaders(input: { [_ in string]?: string } | null | undefined): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [key, value] of Object.entries(input ?? {})) {
    if (value !== undefined) out[key] = value;
  }
  return out;
}

// AddPage is the content of the dedicated, separate add/download window. After
// the user starts a download it stays open and shows live multi-thread progress
// (IDM-style) instead of returning to the main window.
export default function AddPage() {
  useLang(); // re-render on language change
  const [mode, setMode] = useState<Mode>("form");

  const [url, setUrl] = useState("");
  const [filename, setFilename] = useState("");
  const [category, setCategory] = useState("");
  const [saveDir, setSaveDir] = useState("");
  const [dirEdited, setDirEdited] = useState(false);
  const [connections, setConnections] = useState(8);
  const [size, setSize] = useState(-1);
  const [resumable, setResumable] = useState<boolean | null>(null);
  const [categories, setCategories] = useState<string[]>([]);
  const [probing, setProbing] = useState(false);
  const [error, setError] = useState("");
  const [autoName, setAutoName] = useState("");
  const [headers, setHeaders] = useState<Record<string, string>>({});
  const [proxy, setProxy] = useState<ProxySettings>(defaultProxy);
  const [rememberProxy, setRememberProxy] = useState(false);

  const [task, setTask] = useState<TaskInfo | null>(null);

  const fillDir = useCallback(async (cat: string) => {
    if (dirEdited) return;
    try {
      const dir = await api.resolveSaveDir(cat);
      if (dir) setSaveDir(dir);
    } catch {
      /* ignore */
    }
  }, [dirEdited]);

  const probe = useCallback(async (rawURL: string, reqHeaders = headers, reqProxy = proxy) => {
    const trimmed = rawURL.trim();
    if (!trimmed) return;
    setProbing(true);
    setError("");
    try {
      const r = await api.probeURL({ url: trimmed, headers: reqHeaders, proxy: reqProxy });
      setFilename((cur) => (!cur || cur === autoName ? r.filename : cur));
      setAutoName(r.filename);
      setCategory(r.category);
      setSize(r.totalSize);
      setResumable(r.resumable);
      fillDir(r.category);
    } catch (e: any) {
      setError(String(e?.message ?? e));
    } finally {
      setProbing(false);
    }
  }, [autoName, fillDir, headers, proxy]);

  const loadPrefill = useCallback(async (prefillProxy = proxy) => {
    const p = await api.consumePendingAdd(winName);
    if (!p?.url) return;
    const nextHeaders = cleanHeaders(p.headers);
    setUrl(p.url);
    setHeaders(nextHeaders);
    if (p.filename) {
      setFilename(p.filename);
      setAutoName(p.filename);
    }
    probe(p.url, nextHeaders, prefillProxy);
  }, [probe, proxy]);

  useEffect(() => {
    api.categories().then((c) => setCategories(c ?? []));
    api.getSettings().then((s) => {
      setLang(s.language);
      const nextProxy = {
        mode: s.proxy.mode || ("system" as ProxySettings["mode"]),
        url: s.proxy.url || "",
        username: s.proxy.username || "",
        password: s.proxy.password || "",
      } as ProxySettings;
      setConnections(s.connections);
      setProxy(nextProxy);
      loadPrefill(nextProxy);
    });
    fillDir("");
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    if (mode !== "progress" || !task) return;
    return onEvent<TaskInfo>(EVT_TASK_UPDATE, (t) => {
      if (t.id === task.id) setTask(t);
    });
  }, [mode, task]);

  const close = () => Window.Close();

  const onCategoryChange = (c: string) => {
    setCategory(c);
    if (!dirEdited) fillDir(c);
  };

  const request = (autoStart: boolean) => ({
    url: url.trim(),
    filename: filename.trim(),
    category,
    saveDir,
    connections,
    headers,
    proxy,
    rememberProxy,
    autoStart,
  });

  const start = async () => {
    if (!url.trim()) {
      setError(tr("add.needUrl"));
      return;
    }
    try {
      const info = await api.addURL(request(true));
      setTask(info);
      setMode("progress");
    } catch (e: any) {
      setError(String(e?.message ?? e));
    }
  };

  const later = async () => {
    if (!url.trim()) {
      setError(tr("add.needUrl"));
      return;
    }
    try {
      await api.addURL(request(false));
      close();
    } catch (e: any) {
      setError(String(e?.message ?? e));
    }
  };

  const pickFolder = async () => {
    const dir = await api.chooseFolder();
    if (dir) {
      setSaveDir(dir);
      setDirEdited(true);
    }
  };

  if (mode === "progress" && task) {
    return <ProgressView task={task} onClose={close} />;
  }

  return (
    <div className="addwin">
      <div className="addwin-body">
        <div className="field">
          <label>{tr("add.url")}</label>
          <div className="row">
            <input
              type="text"
              value={url}
              placeholder="https://..."
              autoFocus
              onChange={(e) => setUrl(e.target.value)}
              onBlur={(e) => probe(e.target.value)}
            />
            <button className="btn" onClick={() => probe(url)} disabled={probing}>
              {probing ? tr("add.detecting") : tr("add.detect")}
            </button>
          </div>
        </div>

        <div className="field">
          <label>{tr("add.filename")}</label>
          <input type="text" value={filename} onChange={(e) => setFilename(e.target.value)} />
          <span className="hint">
            {tr("add.sizeLabel", { size: formatBytes(size) })}
            {resumable !== null && <> · {resumable ? tr("add.resumableYes") : tr("add.resumableNo")}</>}
          </span>
        </div>

        <div className="field">
          <label>{tr("add.category")}</label>
          <select value={category} onChange={(e) => onCategoryChange(e.target.value)}>
            <option value="">{tr("add.categoryAuto")}</option>
            {categories.map((c) => <option key={c} value={c}>{categoryLabel(c)}</option>)}
          </select>
        </div>

        <div className="field">
          <label>{tr("add.saveTo")}</label>
          <div className="row">
            <input
              type="text"
              value={saveDir}
              onChange={(e) => {
                setSaveDir(e.target.value);
                setDirEdited(true);
              }}
            />
            <button className="btn" onClick={pickFolder}>{tr("common.browse")}</button>
          </div>
          <span className="hint">{tr("add.fullPath", { path: saveDir ? `${saveDir}\\${filename || tr("add.placeholderName")}` : tr("add.pathUnset") })}</span>
        </div>

        <div className="field">
          <label>{tr("add.connections")}</label>
          <input
            type="number"
            min={1}
            max={32}
            value={connections}
            onChange={(e) => setConnections(Math.max(1, Math.min(32, Number(e.target.value))))}
          />
        </div>

        <div className="field">
          <label>{tr("add.proxyMode")}</label>
          <select value={proxy.mode || "system"} onChange={(e) => setProxy({ ...proxy, mode: e.target.value as ProxySettings["mode"] })}>
            <option value="system">{tr("proxy.system")}</option>
            <option value="none">{tr("proxy.none")}</option>
            <option value="custom">{tr("proxy.custom")}</option>
          </select>
        </div>

        {proxy.mode === "custom" && (
          <>
            <div className="field">
              <label>{tr("add.proxyUrl")}</label>
              <input
                type="text"
                placeholder="http://127.0.0.1:7890 / socks5://127.0.0.1:1080"
                value={proxy.url || ""}
                onChange={(e) => setProxy({ ...proxy, url: e.target.value })}
              />
            </div>
            <div className="field">
              <label>{tr("add.proxyAuth")}</label>
              <div className="row">
                <input
                  type="text"
                  placeholder={tr("add.username")}
                  value={proxy.username || ""}
                  onChange={(e) => setProxy({ ...proxy, username: e.target.value })}
                />
                <input
                  type="text"
                  placeholder={tr("add.password")}
                  value={proxy.password || ""}
                  onChange={(e) => setProxy({ ...proxy, password: e.target.value })}
                />
              </div>
            </div>
          </>
        )}

        <div className="field">
          <label className="checkbox">
            <input type="checkbox" checked={rememberProxy} onChange={(e) => setRememberProxy(e.target.checked)} />
            {tr("add.rememberProxy")}
          </label>
        </div>

        {error && <div className="status-text err">{error}</div>}
      </div>

      <div className="addwin-actions">
        <button className="btn" onClick={close}>{tr("common.cancel")}</button>
        <button className="btn" onClick={later}>{tr("add.later")}</button>
        <button className="btn primary" onClick={start}>{tr("add.now")}</button>
      </div>
    </div>
  );
}

function ProgressView({ task: t, onClose }: { task: TaskInfo; onClose: () => void }) {
  const active = t.status === "downloading" || t.status === "connecting";
  const done = t.status === "completed";
  const pct = t.progress >= 0 ? Math.round(t.progress * 100) : -1;
  const segs =
    t.segments && t.segments.length > 0
      ? t.segments
      : Array.from({ length: Math.max(1, t.connections) }, (_, i) => ({
          index: i,
          start: 0,
          end: -1,
          downloaded: 0,
        }));

  return (
    <div className="addwin">
      <div className="addwin-body">
        <div className="info-grid" style={{ marginBottom: 14 }}>
          <span className="k">{tr("pd.filename")}</span><span className="v" title={t.filename}>{t.filename}</span>
          <span className="k">{tr("pd.saveTo")}</span><span className="v" title={t.savePath}>{t.savePath}</span>
          <span className="k">{tr("pd.size")}</span><span className="v">{formatBytes(t.totalSize)}</span>
          <span className="k">{tr("add.downloaded")}</span><span className="v">{formatBytes(t.downloaded)}{pct >= 0 ? `（${pct}%）` : ""}</span>
          <span className="k">{tr("pd.speed")}</span><span className="v">{t.status === "downloading" ? formatSpeed(t.speed) : "-"}</span>
          <span className="k">{tr("add.remaining")}</span><span className="v">{t.status === "downloading" ? formatETA(t.etaSeconds) : "-"}</span>
          <span className="k">{tr("pd.status")}</span><span className="v">{statusLabel(t.status)}{t.error ? ` - ${t.error}` : ""}</span>
        </div>

        <div className="bar" style={{ height: 18 }}>
          <div
            className={`fill${t.status === "paused" ? " paused" : t.status === "error" ? " err" : ""}`}
            style={{ width: pct >= 0 ? `${pct}%` : "0%" }}
          />
          <div className="label">{pct >= 0 ? `${pct}%` : statusLabel(t.status)}</div>
        </div>

        <div style={{ marginTop: 14, fontSize: 12, color: "#44505f", fontWeight: 500 }}>
          {tr("add.threads", { n: segs.length })}
        </div>
        <div className="seg-list">
          {segs.map((s) => {
            const total = s.end - s.start + 1;
            const segPct = total > 0 ? Math.round((s.downloaded / total) * 100) : 0;
            const segActive = active && segPct < 100;
            return (
              <div className="seg-row" key={s.index}>
                <span className="seg-idx">
                  <span className={`seg-dot ${segPct >= 100 ? "ok" : segActive ? "run" : "idle"}`} />
                  {tr("pd.thread", { n: s.index + 1 })}
                </span>
                <span className="seg-bar"><span className="seg-fill" style={{ width: `${segPct}%` }} /></span>
                <span className="seg-pct">{segPct}%</span>
              </div>
            );
          })}
        </div>
      </div>

      <div className="addwin-actions">
        {done ? (
          <>
            <button className="btn" onClick={() => api.openFile(t.id)}>{tr("add.openFile")}</button>
            <button className="btn" onClick={() => api.openFolder(t.id)}>{tr("add.openFolder")}</button>
          </>
        ) : active ? (
          <button className="btn" onClick={() => api.pauseTask(t.id)}>{tr("common.pause")}</button>
        ) : (
          <button className="btn" onClick={() => api.startTask(t.id)}>{tr("common.resume")}</button>
        )}
        {!done && <button className="btn danger" onClick={() => { api.removeTask(t.id, true); onClose(); }}>{tr("add.cancelDownload")}</button>}
        <button className="btn primary" onClick={onClose}>{done ? tr("common.close") : tr("add.hide")}</button>
      </div>
    </div>
  );
}
