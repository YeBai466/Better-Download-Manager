import { api } from "../api";

export interface UpdateInfo {
  current: string;
  latest: string;
  hasUpdate: boolean;
  notes: string;
  releaseUrl: string;
  downloadUrl: string;
  publishedAt: string;
}

interface Props {
  info: UpdateInfo;
  onClose: () => void;
}

// UpdateDialog shows the new version and its release notes (changelog). It does
// not auto-update — the user downloads the installer; reinstalling preserves all
// data/settings (they live in %AppData%, not the install dir).
export default function UpdateDialog({ info, onClose }: Props) {
  const download = () => {
    api.openURL(info.downloadUrl || info.releaseUrl);
  };

  return (
    <div className="overlay" onMouseDown={onClose}>
      <div className="dialog" style={{ width: 560 }} onMouseDown={(e) => e.stopPropagation()}>
        <div className="titlebar">发现新版本</div>
        <div className="content">
          <div className="info-grid" style={{ marginBottom: 14 }}>
            <span className="k">当前版本</span><span className="v">{info.current}</span>
            <span className="k">最新版本</span><span className="v" style={{ color: "#2a9d4a", fontWeight: 600 }}>{info.latest}</span>
          </div>
          <div style={{ fontSize: 12, color: "#44505f", fontWeight: 500, marginBottom: 6 }}>更新内容</div>
          <div className="changelog">{info.notes || "（无更新说明）"}</div>
          <div className="note" style={{ marginTop: 12 }}>
            下载安装包后直接运行覆盖安装即可，<strong>你的下载记录与所有设置都会保留</strong>（保存在用户目录，不随程序更新清除）。
          </div>
        </div>
        <div className="actions">
          <button className="btn" onClick={onClose}>稍后</button>
          {info.releaseUrl && <button className="btn" onClick={() => api.openURL(info.releaseUrl)}>查看发布页</button>}
          <button className="btn primary" onClick={download}>下载更新</button>
        </div>
      </div>
    </div>
  );
}
