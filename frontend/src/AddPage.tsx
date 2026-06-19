import { useEffect, useState, useCallback } from "react";
import { Window } from "@wailsio/runtime";
import "./styles.css";
import { api, onEvent, EVT_TASK_UPDATE, type TaskInfo } from "./api";
import { formatBytes, formatSpeed, formatETA, statusLabels } from "./format";
import { categoryLabel } from "./components/Sidebar";

type Mode = "form" | "progress";

// AddPage is the content of the dedicated, separate add/download window. After
// the user starts a download it stays open and shows live multi-thread progress
// (IDM-style) instead of returning to the main window.
export default function AddPage() {
  const [mode, setMode] = useState<Mode>("form");

  // form state
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

  // progress state
  const [task, setTask] = useState<TaskInfo | null>(null);

  const fillDir = useCallback(async (cat: string) => {
    if (dirEdited) return;
    try {
      const dir = await api.resolveSaveDir(cat);
      if (dir) setSaveDir(dir);
    } catch { /* ignore */ }
  }, [dirEdited]);

  const probe = useCallback(async (u: string) => {
    if (!u.trim()) return;
    setProbing(true);
    setError("");
    try {
      const r = await api.probeURL(u.trim());
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
  }, [autoName, fillDir]);

  const loadPrefill = useCallback(async () => {
    const p = await api.consumePendingAdd();
    if (p?.url) {
      setUrl(p.url);
      if (p.filename) { setFilename(p.filename); setAutoName(p.filename); }
      probe(p.url);
    }
  }, [probe]);

  useEffect(() => {
    api.categories().then((c) => setCategories(c ?? []));
    api.getSettings().then((s) => setConnections(s.connections));
    fillDir(""); // show a real default path immediately
    loadPrefill();
    const off = onEvent("add:reload", () => loadPrefill());
    return off;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // In progress mode, keep the shown task in sync with engine updates.
  useEffect(() => {
    if (mode !== "progress" || !task) return;
    const off = onEvent<TaskInfo>(EVT_TASK_UPDATE, (t) => {
      if (t.id === task.id) setTask(t);
    });
    return off;
  }, [mode, task]);

  const close = () => Window.Close();

  const onCategoryChange = (c: string) => {
    setCategory(c);
    if (!dirEdited) fillDir(c);
  };

  const start = async () => {
    if (!url.trim()) { setError("请输入下载地址"); return; }
    try {
      const info = await api.addURL({
        url: url.trim(), filename: filename.trim(), category, saveDir, connections, headers: {}, autoStart: true,
      });
      setTask(info);
      setMode("progress");
    } catch (e: any) {
      setError(String(e?.message ?? e));
    }
  };

  const later = async () => {
    if (!url.trim()) { setError("请输入下载地址"); return; }
    try {
      await api.addURL({
        url: url.trim(), filename: filename.trim(), category, saveDir, connections, headers: {}, autoStart: false,
      });
      close();
    } catch (e: any) {
      setError(String(e?.message ?? e));
    }
  };

  const pickFolder = async () => {
    const dir = await api.chooseFolder();
    if (dir) { setSaveDir(dir); setDirEdited(true); }
  };

  if (mode === "progress" && task) {
    return <ProgressView task={task} onClose={close} />;
  }

  return (
    <div className="addwin">
      <div className="addwin-body">
        <div className="field">
          <label>下载地址</label>
          <div className="row">
            <input type="text" value={url} placeholder="https://..." autoFocus
              onChange={(e) => setUrl(e.target.value)} onBlur={(e) => probe(e.target.value)} />
            <button className="btn" onClick={() => probe(url)} disabled={probing}>{probing ? "检测中…" : "检测"}</button>
          </div>
        </div>

        <div className="field">
          <label>文件名</label>
          <input type="text" value={filename} onChange={(e) => setFilename(e.target.value)} />
          <span className="hint">
            大小：{formatBytes(size)}
            {resumable !== null && <>　·　{resumable ? "支持断点续传（多线程）" : "不支持续传（单线程）"}</>}
          </span>
        </div>

        <div className="field">
          <label>分类</label>
          <select value={category} onChange={(e) => onCategoryChange(e.target.value)}>
            <option value="">（自动识别）</option>
            {categories.map((c) => <option key={c} value={c}>{categoryLabel(c)}</option>)}
          </select>
        </div>

        <div className="field">
          <label>保存到（不存在的目录会自动创建）</label>
          <div className="row">
            <input type="text" value={saveDir}
              onChange={(e) => { setSaveDir(e.target.value); setDirEdited(true); }} />
            <button className="btn" onClick={pickFolder}>浏览…</button>
          </div>
          <span className="hint">完整路径：{saveDir ? `${saveDir}\\${filename || "（文件名）"}` : "（未设置）"}</span>
        </div>

        <div className="field">
          <label>连接数（线程）</label>
          <input type="number" min={1} max={32} value={connections}
            onChange={(e) => setConnections(Math.max(1, Math.min(32, Number(e.target.value))))} />
        </div>

        {error && <div className="status-text err">{error}</div>}
      </div>

      <div className="addwin-actions">
        <button className="btn" onClick={close}>取消</button>
        <button className="btn" onClick={later}>稍后下载</button>
        <button className="btn primary" onClick={start}>立即下载</button>
      </div>
    </div>
  );
}

function ProgressView({ task: t, onClose }: { task: TaskInfo; onClose: () => void }) {
  const active = t.status === "downloading" || t.status === "connecting";
  const done = t.status === "completed";
  const pct = t.progress >= 0 ? Math.round(t.progress * 100) : -1;

  return (
    <div className="addwin">
      <div className="addwin-body">
        <div className="info-grid" style={{ marginBottom: 14 }}>
          <span className="k">文件名</span><span className="v" title={t.filename}>{t.filename}</span>
          <span className="k">保存到</span><span className="v" title={t.savePath}>{t.savePath}</span>
          <span className="k">大小</span><span className="v">{formatBytes(t.totalSize)}</span>
          <span className="k">已下载</span><span className="v">{formatBytes(t.downloaded)}{pct >= 0 ? `（${pct}%）` : ""}</span>
          <span className="k">速度</span><span className="v">{t.status === "downloading" ? formatSpeed(t.speed) : "—"}</span>
          <span className="k">剩余</span><span className="v">{t.status === "downloading" ? formatETA(t.etaSeconds) : "—"}</span>
          <span className="k">状态</span><span className="v">{statusLabels[t.status] ?? t.status}{t.error ? ` — ${t.error}` : ""}</span>
        </div>

        <div className="bar" style={{ height: 18 }}>
          <div className={`fill${t.status === "paused" ? " paused" : t.status === "error" ? " err" : ""}`}
            style={{ width: pct >= 0 ? `${pct}%` : "0%" }} />
          <div className="label">{pct >= 0 ? `${pct}%` : statusLabels[t.status]}</div>
        </div>

        <div style={{ marginTop: 14, fontSize: 12, color: "#44505f", fontWeight: 500 }}>
          多线程连接（{t.segments?.length ?? 0}）
        </div>
        <div className="seg-list">
          {(t.segments ?? []).map((s) => {
            const total = s.end - s.start + 1;
            const segPct = total > 0 ? Math.round((s.downloaded / total) * 100) : 0;
            const segActive = active && segPct < 100;
            return (
              <div className="seg-row" key={s.index}>
                <span className="seg-idx">
                  <span className={`seg-dot ${segPct >= 100 ? "ok" : segActive ? "run" : "idle"}`} />
                  线程 {s.index + 1}
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
          <button className="btn" onClick={() => api.openFile(t.id)}>打开文件</button>
        ) : active ? (
          <button className="btn" onClick={() => api.pauseTask(t.id)}>暂停</button>
        ) : (
          <button className="btn" onClick={() => api.startTask(t.id)}>继续</button>
        )}
        <button className="btn danger" onClick={() => { api.removeTask(t.id, true); onClose(); }}>取消下载</button>
        <button className="btn primary" onClick={onClose}>隐藏（后台继续）</button>
      </div>
    </div>
  );
}
