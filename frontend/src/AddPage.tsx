import { useCallback, useEffect, useRef, useState } from "react";
import { Window } from "@wailsio/runtime";
import "./styles.css";
import {
  api,
  onEvent,
  EVT_TASK_UPDATE,
  type Settings,
  type TaskInfo,
} from "./api";
import { formatBytes, formatSpeed, formatETA, statusLabel } from "./format";
import { categoryLabel } from "./components/Sidebar";
import { t as tr, setLang, useLang } from "./i18n";

type Mode = "form" | "progress";
type ProxySettings = Settings["proxy"];

const defaultProxy = {
  mode: "system",
  url: "",
  username: "",
  password: "",
} as ProxySettings;

// Each add-download window is opened with a unique name in the URL (?w=add-N);
// the prefill is keyed by that name so concurrent windows don't read each
// other's data.
const winName = new URLSearchParams(window.location.search).get("w") ?? "";

function cleanHeaders(
  input: { [_ in string]?: string } | null | undefined,
): Record<string, string> {
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
  const dirEdited = useRef(false);
  const nameEdited = useRef(false);
  const categoryEdited = useRef(false);
  const probeSeq = useRef(0);
  const dirSeq = useRef(0);
  const submitting = useRef(false);
  const [busy, setBusy] = useState(false);
  const [handoffId, setHandoffId] = useState("");
  const [connections, setConnections] = useState(8);
  const [size, setSize] = useState(-1);
  const [resumable, setResumable] = useState<boolean | null>(null);
  const [categories, setCategories] = useState<string[]>([]);
  const [probing, setProbing] = useState(false);
  const [error, setError] = useState("");

  const [headers, setHeaders] = useState<Record<string, string>>({});
  const [proxy, setProxy] = useState<ProxySettings>(defaultProxy);
  const [rememberProxy, setRememberProxy] = useState(false);
  const changeProxy = (value: ProxySettings) => {
    probeSeq.current++;
    setProbing(false);
    setProxy(value);
  };
  useEffect(() => {
    const handler = (event: PromiseRejectionEvent) => {
      event.preventDefault();
      setError(String(event.reason?.message ?? event.reason));
    };
    window.addEventListener("unhandledrejection", handler);
    return () => window.removeEventListener("unhandledrejection", handler);
  }, []);

  const [task, setTask] = useState<TaskInfo | null>(null);

  const fillDir = useCallback(async (cat: string) => {
    if (dirEdited.current) return;
    const sequence = ++dirSeq.current;
    try {
      const dir = await api.resolveSaveDir(cat);
      if (dir && sequence === dirSeq.current && !dirEdited.current)
        setSaveDir(dir);
    } catch {
      /* ignore */
    }
  }, []);

  const probe = useCallback(
    async (rawURL: string, reqHeaders = headers, reqProxy = proxy) => {
      const trimmed = rawURL.trim();
      const sequence = ++probeSeq.current;
      if (!trimmed) {
        setSize(-1);
        setResumable(null);
        return;
      }
      setProbing(true);
      setError("");
      try {
        const r = await api.probeURL({
          url: trimmed,
          headers: reqHeaders,
          proxy: reqProxy,
        });
        if (sequence !== probeSeq.current) return;
        if (!nameEdited.current) setFilename(r.filename);
        if (!categoryEdited.current) setCategory(r.category);
        setSize(r.totalSize);
        setResumable(r.resumable);
        if (!categoryEdited.current) fillDir(r.category);
      } catch (e: any) {
        if (sequence === probeSeq.current) setError(String(e?.message ?? e));
      } finally {
        if (sequence === probeSeq.current) setProbing(false);
      }
    },
    [fillDir, headers, proxy],
  );

  const initial = useRef<Promise<
    [
      string[] | null,
      Settings,
      Awaited<ReturnType<typeof api.consumePendingAdd>>,
    ]
  > | null>(null);
  useEffect(() => {
    let active = true;
    if (!initial.current)
      initial.current = Promise.all([
        api.categories(),
        api.getSettings(),
        api.consumePendingAdd(winName),
      ]);
    initial
      .current!.then(([cats, s, p]) => {
        if (!active) return;
        setCategories(cats ?? []);
        setLang(s.language);
        setConnections(s.connections);
        const nextProxy = { ...defaultProxy, ...s.proxy };
        setProxy(nextProxy);
        if (p?.url) {
          const nextHeaders = cleanHeaders(p.headers);
          setHeaders(nextHeaders);
          setUrl(p.url);
          setHandoffId(p.handoffId ?? "");
          if (p.filename) setFilename(p.filename);
          probe(p.url, nextHeaders, nextProxy);
        } else fillDir("");
      })
      .catch((error) => {
        if (active) setError(String(error));
      });
    const off = onEvent<Settings>("settings:changed", (s) =>
      setLang(s.language),
    );
    return () => {
      active = false;
      probeSeq.current++;
      off();
    };
  }, []);

  const taskId = task?.id;
  useEffect(() => {
    if (mode !== "progress" || !taskId) return;
    let active = true;
    const accept = (next: TaskInfo) => {
      if (active && next.id === taskId)
        setTask((old) => (!old || next.revision >= old.revision ? next : old));
    };
    const off = onEvent<TaskInfo>(EVT_TASK_UPDATE, accept);
    api
      .getTask(taskId)
      .then(accept)
      .catch((error) => {
        if (active) setError(String(error));
      });
    return () => {
      active = false;
      off();
    };
  }, [mode, taskId]);

  const close = async () => {
    if (mode === "form") await api.cancelHandoff(handoffId);
    await Window.Close();
  };

  const onCategoryChange = (c: string) => {
    categoryEdited.current = true;
    setCategory(c);
    if (!dirEdited.current) fillDir(c);
  };

  const request = (autoStart: boolean) => ({
    handoffId,
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

  const submit = async (autoStart: boolean) => {
    if (submitting.current) return;
    if (!url.trim()) {
      setError(tr("add.needUrl"));
      return;
    }
    submitting.current = true;
    setBusy(true);
    probeSeq.current++;
    try {
      const info = await api.addURL(request(autoStart));
      if (autoStart) {
        setTask(info);
        setMode("progress");
      } else await Window.Close();
    } catch (error: any) {
      setError(String(error?.message ?? error));
    } finally {
      submitting.current = false;
      setBusy(false);
    }
  };
  const start = () => submit(true);
  const later = () => submit(false);

  const pickFolder = async () => {
    const dir = await api.chooseFolder();
    if (dir) {
      setSaveDir(dir);
      dirEdited.current = true;
      dirSeq.current++;
    }
  };

  if (mode === "progress" && task) {
    return <ProgressView task={task} onClose={close} error={error} />;
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
              onChange={(e) => {
                probeSeq.current++;
                setProbing(false);
                setUrl(e.target.value);
                setSize(-1);
                setResumable(null);
              }}
              onBlur={(e) => probe(e.target.value)}
            />
            <button
              className="btn"
              onClick={() => probe(url)}
              disabled={probing}
            >
              {probing ? tr("add.detecting") : tr("add.detect")}
            </button>
          </div>
        </div>

        <div className="field">
          <label>{tr("add.filename")}</label>
          <input
            type="text"
            value={filename}
            onChange={(e) => {
              nameEdited.current = true;
              setFilename(e.target.value);
            }}
          />
          <span className="hint">
            {tr("add.sizeLabel", { size: formatBytes(size) })}
            {resumable !== null && (
              <>
                {" "}
                · {resumable ? tr("add.resumableYes") : tr("add.resumableNo")}
              </>
            )}
          </span>
        </div>

        <div className="field">
          <label>{tr("add.category")}</label>
          <select
            value={category}
            onChange={(e) => onCategoryChange(e.target.value)}
          >
            <option value="">{tr("add.categoryAuto")}</option>
            {categories.map((c) => (
              <option key={c} value={c}>
                {categoryLabel(c)}
              </option>
            ))}
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
                dirEdited.current = true;
                dirSeq.current++;
              }}
            />
            <button className="btn" onClick={pickFolder}>
              {tr("common.browse")}
            </button>
          </div>
          <span className="hint">
            {tr("add.fullPath", {
              path: saveDir
                ? `${saveDir}\\${filename || tr("add.placeholderName")}`
                : tr("add.pathUnset"),
            })}
          </span>
        </div>

        <div className="field">
          <label>{tr("add.connections")}</label>
          <input
            type="number"
            min={1}
            max={32}
            value={connections}
            onChange={(e) =>
              setConnections(Math.max(1, Math.min(32, Number(e.target.value))))
            }
          />
        </div>

        <div className="field">
          <label>{tr("add.proxyMode")}</label>
          <select
            value={proxy.mode || "system"}
            onChange={(e) =>
              changeProxy({
                ...proxy,
                mode: e.target.value as ProxySettings["mode"],
              })
            }
          >
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
                onChange={(e) => changeProxy({ ...proxy, url: e.target.value })}
              />
            </div>
            <div className="field">
              <label>{tr("add.proxyAuth")}</label>
              <div className="row">
                <input
                  type="text"
                  placeholder={tr("add.username")}
                  value={proxy.username || ""}
                  onChange={(e) =>
                    changeProxy({ ...proxy, username: e.target.value })
                  }
                />
                <input
                  type="password"
                  placeholder={tr("add.password")}
                  value={proxy.password || ""}
                  onChange={(e) =>
                    changeProxy({ ...proxy, password: e.target.value })
                  }
                />
              </div>
            </div>
          </>
        )}

        <div className="field">
          <label className="checkbox">
            <input
              type="checkbox"
              checked={rememberProxy}
              onChange={(e) => setRememberProxy(e.target.checked)}
            />
            {tr("add.rememberProxy")}
          </label>
        </div>

        {error && <div className="status-text err">{error}</div>}
      </div>

      <div className="addwin-actions">
        <button className="btn" onClick={close} disabled={busy}>
          {tr("common.cancel")}
        </button>
        <button className="btn" onClick={later} disabled={busy}>
          {tr("add.later")}
        </button>
        <button className="btn primary" onClick={start} disabled={busy}>
          {tr("add.now")}
        </button>
      </div>
    </div>
  );
}

function ProgressView({
  task: t,
  onClose,
  error,
}: {
  task: TaskInfo;
  onClose: () => void;
  error: string;
}) {
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
        {error && (
          <div role="alert" className="status-text err">
            {error}
          </div>
        )}
        <div className="info-grid" style={{ marginBottom: 14 }}>
          <span className="k">{tr("pd.filename")}</span>
          <span className="v" title={t.filename}>
            {t.filename}
          </span>
          <span className="k">{tr("pd.saveTo")}</span>
          <span className="v" title={t.savePath}>
            {t.savePath}
          </span>
          <span className="k">{tr("pd.size")}</span>
          <span className="v">{formatBytes(t.totalSize)}</span>
          <span className="k">{tr("add.downloaded")}</span>
          <span className="v">
            {formatBytes(t.downloaded)}
            {pct >= 0 ? `（${pct}%）` : ""}
          </span>
          <span className="k">{tr("pd.speed")}</span>
          <span className="v">
            {t.status === "downloading" ? formatSpeed(t.speed) : "-"}
          </span>
          <span className="k">{tr("add.remaining")}</span>
          <span className="v">
            {t.status === "downloading" ? formatETA(t.etaSeconds) : "-"}
          </span>
          <span className="k">{tr("pd.status")}</span>
          <span className="v">
            {statusLabel(t.status)}
            {t.error ? ` - ${t.error}` : ""}
          </span>
        </div>

        <div className="bar" style={{ height: 18 }}>
          <div
            className={`fill${t.status === "paused" ? " paused" : t.status === "error" ? " err" : ""}`}
            style={{ width: pct >= 0 ? `${pct}%` : "0%" }}
          />
          <div className="label">
            {pct >= 0 ? `${pct}%` : statusLabel(t.status)}
          </div>
        </div>

        <div
          style={{
            marginTop: 14,
            fontSize: 12,
            color: "#44505f",
            fontWeight: 500,
          }}
        >
          {tr("add.threads", { n: segs.length })}
        </div>
        <div className="seg-list">
          {segs.map((s) => {
            const total = s.end - s.start + 1;
            const segPct =
              t.status === "completed"
                ? 100
                : total > 0
                  ? Math.min(100, Math.round((s.downloaded / total) * 100))
                  : 0;
            const segActive = active && segPct < 100;
            return (
              <div className="seg-row" key={s.index}>
                <span className="seg-idx">
                  <span
                    className={`seg-dot ${segPct >= 100 ? "ok" : segActive ? "run" : "idle"}`}
                  />
                  {tr("pd.thread", { n: s.index + 1 })}
                </span>
                <span className="seg-bar">
                  <span className="seg-fill" style={{ width: `${segPct}%` }} />
                </span>
                <span className="seg-pct">{segPct}%</span>
              </div>
            );
          })}
        </div>
      </div>

      <div className="addwin-actions">
        {done ? (
          <>
            <button className="btn" onClick={() => api.openFile(t.id)}>
              {tr("add.openFile")}
            </button>
            <button className="btn" onClick={() => api.openFolder(t.id)}>
              {tr("add.openFolder")}
            </button>
          </>
        ) : active ? (
          <button className="btn" onClick={() => api.pauseTask(t.id)}>
            {tr("common.pause")}
          </button>
        ) : (
          <button className="btn" onClick={() => api.startTask(t.id)}>
            {tr("common.resume")}
          </button>
        )}
        {!done && (
          <button
            className="btn danger"
            onClick={() => {
              api.removeTask(t.id, true);
              onClose();
            }}
          >
            {tr("add.cancelDownload")}
          </button>
        )}
        <button className="btn primary" onClick={onClose}>
          {done ? tr("common.close") : tr("add.hide")}
        </button>
      </div>
    </div>
  );
}
