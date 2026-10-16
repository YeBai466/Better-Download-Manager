import type { TaskInfo } from "../api";
import { formatBytes, formatSpeed, formatETA, statusLabel } from "../format";
import { t as tr } from "../i18n";

interface Props {
  task: TaskInfo;
  onResume: (id: string) => void;
  onPause: (id: string) => void;
  onClose: () => void;
}

export default function ProgressDialog({
  task: t,
  onResume,
  onPause,
  onClose,
}: Props) {
  const active = t.status === "downloading" || t.status === "connecting";
  const pct = t.progress >= 0 ? Math.round(t.progress * 100) : -1;
  // While connecting, segments aren't planned yet — show placeholder rows for
  // the intended connection count so the multi-thread view is alive immediately.
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
    <div className="overlay" onMouseDown={onClose}>
      <div
        className="dialog progress-dialog"
        onMouseDown={(e) => e.stopPropagation()}
      >
        <div className="titlebar">{tr("pd.title")}</div>
        <div className="content">
          <div className="info-grid" style={{ marginBottom: 16 }}>
            <span className="k">{tr("pd.filename")}</span>
            <span className="v" title={t.filename}>
              {t.filename}
            </span>
            <span className="k">{tr("pd.saveTo")}</span>
            <span className="v" title={t.savePath}>
              {t.savePath}
            </span>
            <span className="k">{tr("pd.url")}</span>
            <span className="v" title={t.url}>
              {t.url}
            </span>
            <span className="k">{tr("pd.size")}</span>
            <span className="v">{formatBytes(t.totalSize)}</span>
            <span className="k">{tr("pd.downloaded")}</span>
            <span className="v">
              {formatBytes(t.downloaded)}
              {pct >= 0 ? `（${pct}%）` : ""}
            </span>
            <span className="k">{tr("pd.status")}</span>
            <span className="v">
              {statusLabel(t.status)}
              {t.error ? ` — ${t.error}` : ""}
            </span>
            <span className="k">{tr("pd.speed")}</span>
            <span className="v">
              {t.status === "downloading" ? formatSpeed(t.speed) : "—"}
            </span>
            <span className="k">{tr("pd.eta")}</span>
            <span className="v">
              {t.status === "downloading" ? formatETA(t.etaSeconds) : "—"}
            </span>
            <span className="k">{tr("pd.resumable")}</span>
            <span className="v">
              {t.resumable ? tr("common.yes") : tr("common.no")}
            </span>
          </div>

          <div className="bar" style={{ height: 18 }}>
            <div
              className={`fill${t.status === "paused" ? "" : ""}`}
              style={{ width: pct >= 0 ? `${pct}%` : "0%" }}
            />
            <div className="label">
              {pct >= 0 ? `${pct}%` : statusLabel(t.status)}
            </div>
          </div>

          <div
            style={{
              marginTop: 16,
              fontSize: 12,
              color: "#44505f",
              fontWeight: 500,
            }}
          >
            {tr("pd.segments", { n: segs.length })}
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
              return (
                <div className="seg-row" key={s.index}>
                  <span className="seg-idx">
                    {tr("pd.thread", { n: s.index + 1 })}
                  </span>
                  <span className="seg-bar">
                    <span
                      className="seg-fill"
                      style={{ width: `${segPct}%` }}
                    />
                  </span>
                  <span className="seg-pct">{segPct}%</span>
                </div>
              );
            })}
          </div>
        </div>
        <div className="actions">
          {active ? (
            <button className="btn" onClick={() => onPause(t.id)}>
              {tr("common.pause")}
            </button>
          ) : (
            t.status !== "completed" && (
              <button className="btn" onClick={() => onResume(t.id)}>
                {tr("common.resume")}
              </button>
            )
          )}
          <button className="btn primary" onClick={onClose}>
            {tr("common.close")}
          </button>
        </div>
      </div>
    </div>
  );
}
