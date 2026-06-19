import type { TaskInfo } from "../api";
import { formatBytes, formatSpeed, formatETA, statusLabels } from "../format";

interface Props {
  task: TaskInfo;
  onResume: (id: string) => void;
  onPause: (id: string) => void;
  onClose: () => void;
}

export default function ProgressDialog({ task: t, onResume, onPause, onClose }: Props) {
  const active = t.status === "downloading" || t.status === "connecting";
  const pct = t.progress >= 0 ? Math.round(t.progress * 100) : -1;

  return (
    <div className="overlay" onMouseDown={onClose}>
      <div className="dialog progress-dialog" onMouseDown={(e) => e.stopPropagation()}>
        <div className="titlebar">下载进度</div>
        <div className="content">
          <div className="info-grid" style={{ marginBottom: 16 }}>
            <span className="k">文件名</span><span className="v" title={t.filename}>{t.filename}</span>
            <span className="k">保存到</span><span className="v" title={t.savePath}>{t.savePath}</span>
            <span className="k">地址</span><span className="v" title={t.url}>{t.url}</span>
            <span className="k">大小</span><span className="v">{formatBytes(t.totalSize)}</span>
            <span className="k">已下载</span><span className="v">{formatBytes(t.downloaded)}{pct >= 0 ? `（${pct}%）` : ""}</span>
            <span className="k">状态</span><span className="v">{statusLabels[t.status] ?? t.status}{t.error ? ` — ${t.error}` : ""}</span>
            <span className="k">速度</span><span className="v">{t.status === "downloading" ? formatSpeed(t.speed) : "—"}</span>
            <span className="k">剩余时间</span><span className="v">{t.status === "downloading" ? formatETA(t.etaSeconds) : "—"}</span>
            <span className="k">支持续传</span><span className="v">{t.resumable ? "是" : "否"}</span>
          </div>

          <div className="bar" style={{ height: 18 }}>
            <div className={`fill${t.status === "paused" ? "" : ""}`} style={{ width: pct >= 0 ? `${pct}%` : "0%" }} />
            <div className="label">{pct >= 0 ? `${pct}%` : statusLabels[t.status]}</div>
          </div>

          <div style={{ marginTop: 16, fontSize: 12, color: "#44505f", fontWeight: 500 }}>
            连接分段（{t.segments?.length ?? 0}）
          </div>
          <div className="seg-list">
            {(t.segments ?? []).map((s) => {
              const total = s.end - s.start + 1;
              const segPct = total > 0 ? Math.round((s.downloaded / total) * 100) : 0;
              return (
                <div className="seg-row" key={s.index}>
                  <span className="seg-idx">线程 {s.index + 1}</span>
                  <span className="seg-bar"><span className="seg-fill" style={{ width: `${segPct}%` }} /></span>
                  <span className="seg-pct">{segPct}%</span>
                </div>
              );
            })}
          </div>
        </div>
        <div className="actions">
          {active ? (
            <button className="btn" onClick={() => onPause(t.id)}>暂停</button>
          ) : (
            t.status !== "completed" && <button className="btn" onClick={() => onResume(t.id)}>继续</button>
          )}
          <button className="btn primary" onClick={onClose}>关闭</button>
        </div>
      </div>
    </div>
  );
}
