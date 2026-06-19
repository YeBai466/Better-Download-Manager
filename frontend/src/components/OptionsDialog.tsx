import { useEffect, useState } from "react";
import { api, type Settings } from "../api";
import ManualInstall from "./ManualInstall";

interface Props {
  onClose: () => void;
  onSaved: (s: Settings) => void;
}

type Tab = "general" | "save" | "connection" | "proxy" | "browser";

const tabs: { id: Tab; label: string }[] = [
  { id: "general", label: "常规" },
  { id: "save", label: "保存与分类" },
  { id: "connection", label: "连接" },
  { id: "proxy", label: "代理" },
  { id: "browser", label: "浏览器接管" },
];

export default function OptionsDialog({ onClose, onSaved }: Props) {
  const [tab, setTab] = useState<Tab>("general");
  const [s, setS] = useState<Settings | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState("");
  const [appVersion, setAppVersion] = useState("");
  const [updateMsg, setUpdateMsg] = useState("");

  useEffect(() => {
    api.getSettings().then(setS);
  }, []);

  if (!s) return null;

  const patch = (p: Partial<Settings>) => setS({ ...s, ...p } as Settings);
  const proxyPatch = (p: Partial<Settings["proxy"]>) => setS({ ...s, proxy: { ...s.proxy, ...p } } as Settings);

  const save = async () => {
    try {
      const saved = await api.saveSettings(s);
      onSaved(saved);
      onClose();
    } catch (e: any) {
      setError(String(e?.message ?? e));
    }
  };

  const pickFolder = async () => {
    const dir = await api.chooseFolder();
    if (dir) patch({ downloadDir: dir });
  };

  const checkUpdate = async () => {
    setBusy("update");
    setUpdateMsg("");
    try {
      const r = await api.checkForUpdates();
      setAppVersion(r.current);
      if (r.hasUpdate) {
        setUpdateMsg(`　·　发现新版本 ${r.latest}`);
        // Open the update window for the changelog + download.
        window.dispatchEvent(new CustomEvent("bdm:update", { detail: r }));
      } else {
        setUpdateMsg("　·　已是最新版本");
      }
    } catch (e: any) {
      setUpdateMsg("　·　检查失败：" + String(e?.message ?? e));
    } finally {
      setBusy("");
    }
  };

  return (
    <div className="overlay" onMouseDown={onClose}>
      <div className="dialog options" onMouseDown={(e) => e.stopPropagation()}>
        <div className="titlebar">选项</div>
        <div className="obody">
          <div className="opt-tabs">
            {tabs.map((t) => (
              <div key={t.id} className={`opt-tab${tab === t.id ? " active" : ""}`} onClick={() => setTab(t.id)}>
                {t.label}
              </div>
            ))}
          </div>
          <div className="opt-pane">
            {tab === "general" && (
              <div className="opt-group">
                <h3>常规</h3>
                <div className="field">
                  <label>默认下载目录</label>
                  <div className="row">
                    <input type="text" value={s.downloadDir} onChange={(e) => patch({ downloadDir: e.target.value })} />
                    <button className="btn" onClick={pickFolder}>浏览…</button>
                  </div>
                  <span className="hint">未指定分类目录的文件将保存到此处。</span>
                </div>
                <div className="field">
                  <label className="checkbox">
                    <input type="checkbox" checked={s.categorize} onChange={(e) => patch({ categorize: e.target.checked })} />
                    按文件类型自动归类到子目录（视频 / 音乐 / 文档…）
                  </label>
                </div>

                <h3 style={{ marginTop: 18 }}>启动</h3>
                <div className="field">
                  <label className="checkbox">
                    <input type="checkbox" checked={s.autoStart} onChange={(e) => patch({ autoStart: e.target.checked })} />
                    开机时自动启动
                  </label>
                </div>
                <div className="field">
                  <label className="checkbox" style={{ opacity: s.autoStart ? 1 : 0.45 }}>
                    <input type="checkbox" disabled={!s.autoStart} checked={s.startMinimized}
                      onChange={(e) => patch({ startMinimized: e.target.checked })} />
                    开机启动时最小化到系统托盘
                  </label>
                </div>

                <h3 style={{ marginTop: 18 }}>更新</h3>
                <div className="field">
                  <label className="checkbox">
                    <input type="checkbox" checked={s.autoCheckUpdate} onChange={(e) => patch({ autoCheckUpdate: e.target.checked })} />
                    启动时自动检查更新（GitHub Releases）
                  </label>
                </div>
                <div className="field">
                  <div className="row">
                    <button className="btn" onClick={checkUpdate} disabled={busy === "update"}>
                      {busy === "update" ? "检查中…" : "立即检查更新"}
                    </button>
                    <span className="hint" style={{ flex: 1 }}>当前版本 {appVersion}{updateMsg}</span>
                  </div>
                </div>
              </div>
            )}

            {tab === "save" && (
              <div className="opt-group">
                <h3>分类保存目录</h3>
                <span className="hint">留空则使用「默认下载目录 / 分类名」。开启上方「自动归类」后生效。</span>
                <div style={{ height: 12 }} />
                {["General", "Compressed", "Documents", "Music", "Video", "Programs"].map((c) => (
                  <div className="field" key={c}>
                    <label>{catName(c)}</label>
                    <div className="row">
                      <input
                        type="text"
                        placeholder={`默认：${s.downloadDir}\\${c}`}
                        value={s.categoryDirs?.[c] ?? ""}
                        onChange={(e) => patch({ categoryDirs: { ...s.categoryDirs, [c]: e.target.value } })}
                      />
                    </div>
                  </div>
                ))}
              </div>
            )}

            {tab === "connection" && (
              <div className="opt-group">
                <h3>连接</h3>
                <div className="field">
                  <label>同时下载的任务数</label>
                  <input type="number" min={1} max={20} value={s.maxConcurrent}
                    onChange={(e) => patch({ maxConcurrent: Number(e.target.value) })} />
                  <span className="hint">超出的任务会进入队列排队。</span>
                </div>
                <div className="field">
                  <label>每个下载的连接数（线程）</label>
                  <input type="number" min={1} max={32} value={s.connections}
                    onChange={(e) => patch({ connections: Number(e.target.value) })} />
                  <span className="hint">服务器支持续传时才会分段，推荐 8～16。</span>
                </div>
                <div className="field">
                  <label>全局限速（KB/s，0 = 不限速）</label>
                  <input type="number" min={0} value={Math.round(s.speedLimit / 1024)}
                    onChange={(e) => patch({ speedLimit: Math.max(0, Number(e.target.value)) * 1024 })} />
                </div>
              </div>
            )}

            {tab === "proxy" && (
              <div className="opt-group">
                <h3>代理</h3>
                <div className="field">
                  <label>代理方式</label>
                  <select value={s.proxy.mode} onChange={(e) => proxyPatch({ mode: e.target.value as any })}>
                    <option value="system">使用系统代理（默认）</option>
                    <option value="none">不使用代理（直连）</option>
                    <option value="custom">自定义代理</option>
                  </select>
                </div>
                {s.proxy.mode === "custom" && (
                  <>
                    <div className="field">
                      <label>代理地址</label>
                      <input type="text" placeholder="http://127.0.0.1:7890 或 socks5://127.0.0.1:1080"
                        value={s.proxy.url} onChange={(e) => proxyPatch({ url: e.target.value })} />
                      <span className="hint">支持 http / https / socks5。</span>
                    </div>
                    <div className="field">
                      <label>认证（可选）</label>
                      <div className="row">
                        <input type="text" placeholder="用户名" value={s.proxy.username}
                          onChange={(e) => proxyPatch({ username: e.target.value })} />
                        <input type="text" placeholder="密码" value={s.proxy.password}
                          onChange={(e) => proxyPatch({ password: e.target.value })} />
                      </div>
                    </div>
                  </>
                )}
              </div>
            )}

            {tab === "browser" && (
              <>
                <div className="opt-group">
                  <h3>浏览器接管</h3>
                  <div className="field">
                    <label className="checkbox">
                      <input type="checkbox" checked={s.takeoverEnabled} onChange={(e) => patch({ takeoverEnabled: e.target.checked })} />
                      启用浏览器接管（本地服务）
                    </label>
                  </div>
                  {s.takeoverEnabled && (
                    <>
                      <div className="field">
                        <label>本地服务端口</label>
                        <input type="number" value={s.takeoverPort} onChange={(e) => patch({ takeoverPort: Number(e.target.value) })} />
                      </div>
                      <div className="field">
                        <label>收到浏览器下载时</label>
                        <select value={s.takeoverAction} onChange={(e) => patch({ takeoverAction: e.target.value as any })}>
                          <option value="dialog">显示「添加下载」对话框</option>
                          <option value="auto">直接开始下载</option>
                        </select>
                      </div>
                    </>
                  )}
                </div>

                <div className="opt-group">
                  <h3>浏览器扩展（Chrome / Edge）</h3>
                  <ManualInstall />
                  <div className="field" style={{ marginTop: 12 }}>
                    <label className="checkbox">
                      <input type="checkbox" checked={!s.extPromptIgnored} onChange={(e) => patch({ extPromptIgnored: !e.target.checked })} />
                      启动时提醒我安装扩展
                    </label>
                  </div>
                  <div className="note">
                    扩展已发布在 Chrome 网上应用店，可一键静默安装（个人电脑同样适用）。
                    Firefox 暂不支持策略安装，请到 <code>about:debugging</code> 手动加载 <code>extensions/firefox</code>。
                  </div>
                </div>
              </>
            )}

            {error && <div className="status-text err" style={{ marginTop: 8 }}>{error}</div>}
          </div>
        </div>
        <div className="actions">
          <button className="btn" onClick={onClose}>取消</button>
          <button className="btn primary" onClick={save}>保存</button>
        </div>
      </div>
    </div>
  );
}

const names: Record<string, string> = {
  General: "常规", Compressed: "压缩文件", Documents: "文档", Music: "音乐", Video: "视频", Programs: "程序",
};
function catName(c: string): string { return names[c] ?? c; }
