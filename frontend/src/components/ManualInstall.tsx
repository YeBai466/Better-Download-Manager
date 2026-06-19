import { useEffect, useState } from "react";
import { api } from "../api";

const BROWSERS = ["Chrome", "Edge"] as const;
type Browser = (typeof BROWSERS)[number];

const extUrl = (b: Browser) => (b === "Edge" ? "edge://extensions" : "chrome://extensions");

// ManualInstall guides a one-time "Load unpacked" install. Browsers block
// opening chrome://extensions from the command line, so instead of trying (and
// failing) to auto-open it, we copy the URL to the clipboard and open the
// extension folder, then show the user exactly what to paste.
export default function ManualInstall() {
  const [installed, setInstalled] = useState<string[]>([]);
  const [target, setTarget] = useState<Browser>("Chrome");
  const [dir, setDir] = useState("");
  const [copied, setCopied] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    api.installedBrowsers().then((b) => {
      const list = b ?? [];
      setInstalled(list);
      const first = BROWSERS.find((x) => list.includes(x));
      if (first) setTarget(first);
    });
  }, []);

  const url = extUrl(target);

  const run = async () => {
    setBusy(true);
    setError("");
    try {
      const info = await api.prepareManualInstall(); // extracts + opens the folder
      setDir(info.dir);
      try {
        await navigator.clipboard.writeText(url);
        setCopied(true);
      } catch {
        setCopied(false);
      }
    } catch (e: any) {
      setError(String(e?.message ?? e));
    } finally {
      setBusy(false);
    }
  };

  const copyUrl = async () => {
    try { await navigator.clipboard.writeText(url); setCopied(true); } catch { /* ignore */ }
  };

  return (
    <div>
      <div className="field">
        <label>要安装到哪个浏览器</label>
        <div className="row" style={{ gap: 18 }}>
          {BROWSERS.map((b) => {
            const has = installed.includes(b);
            return (
              <label key={b} className="checkbox" style={{ opacity: has ? 1 : 0.45 }}>
                <input type="radio" name="ext-target" disabled={!has}
                  checked={target === b} onChange={() => { setTarget(b); setCopied(false); }} />
                {b}{!has && "（未安装）"}
              </label>
            );
          })}
        </div>
      </div>

      <div className="field">
        <button className="btn primary" onClick={run} disabled={busy}>
          {busy ? "准备中…" : "准备扩展文件夹"}
        </button>
      </div>

      {error && <div className="status-text err">{error}</div>}

      {dir && (
        <div className="note">
          <p style={{ margin: "0 0 6px", fontWeight: 600 }}>扩展文件夹已在资源管理器中打开。请按 3 步加载到 {target}：</p>
          <ol style={{ margin: "0 0 10px", paddingLeft: 18, lineHeight: 2 }}>
            <li>
              在 {target} 地址栏粘贴 <code>{url}</code>{copied && "（已复制，直接 Ctrl+V 回车）"} 回车
              <button className="btn" style={{ marginLeft: 8, padding: "2px 10px" }} onClick={copyUrl}>复制地址</button>
            </li>
            <li>打开右上角 <strong>开发者模式</strong>（Developer mode）</li>
            <li>点 <strong>加载已解压的扩展程序</strong>（Load unpacked），选择下面这个文件夹：</li>
          </ol>
          <input type="text" readOnly value={dir} onFocus={(e) => e.currentTarget.select()} style={{ width: "100%" }} />
          <p style={{ margin: "8px 0 0", color: "var(--muted)" }}>
            浏览器内部页（{url}）无法由程序自动打开，这是浏览器的安全限制，所以需要你手动粘贴一次。加载后永久生效。
          </p>
        </div>
      )}
    </div>
  );
}
