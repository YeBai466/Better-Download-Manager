import { useEffect, useState } from "react";
import { api, type Settings } from "../api";
import ManualInstall from "./ManualInstall";
import { t, setLang, useLang, type Lang } from "../i18n";

interface Props {
  onClose: () => void;
  onSaved: (s: Settings) => void;
}

type Tab = "general" | "save" | "connection" | "proxy" | "browser" | "language";

const tabIds: { id: Tab; key: string }[] = [
  { id: "general", key: "opt.tab.general" },
  { id: "save", key: "opt.tab.save" },
  { id: "connection", key: "opt.tab.connection" },
  { id: "proxy", key: "opt.tab.proxy" },
  { id: "browser", key: "opt.tab.browser" },
  { id: "language", key: "opt.tab.language" },
];

export default function OptionsDialog({ onClose, onSaved }: Props) {
  useLang(); // re-render this dialog when the language changes
  const [tab, setTab] = useState<Tab>("general");
  const [s, setS] = useState<Settings | null>(null);
  const [initialLang, setInitialLang] = useState<Lang>("zh");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState("");
  const [appVersion, setAppVersion] = useState("");
  const [updateMsg, setUpdateMsg] = useState("");

  useEffect(() => {
    api.getSettings().then((cfg) => {
      setS(cfg);
      setInitialLang(cfg.language === "en" ? "en" : "zh");
    });
  }, []);

  if (!s) return null;

  const patch = (p: Partial<Settings>) => setS({ ...s, ...p } as Settings);
  const proxyPatch = (p: Partial<Settings["proxy"]>) =>
    setS({ ...s, proxy: { ...s.proxy, ...p } } as Settings);

  // Live-preview the language while in the dialog; persist on Save, revert on Cancel.
  const pickLanguage = (lang: Lang) => {
    patch({ language: lang });
    setLang(lang);
  };

  const cancel = () => {
    setLang(initialLang); // revert any unsaved language preview
    onClose();
  };

  const save = async () => {
    try {
      const saved = await api.saveSettings(s);
      setLang(saved.language);
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
        setUpdateMsg(t("opt.foundNew", { v: r.latest }));
        // Open the update window for the changelog + download.
        window.dispatchEvent(new CustomEvent("bdm:update", { detail: r }));
      } else {
        setUpdateMsg(t("opt.isLatest"));
      }
    } catch (e: any) {
      setUpdateMsg(t("opt.checkFailed", { err: String(e?.message ?? e) }));
    } finally {
      setBusy("");
    }
  };

  return (
    <div className="overlay" onMouseDown={cancel}>
      <div className="dialog options" onMouseDown={(e) => e.stopPropagation()}>
        <div className="titlebar">{t("opt.title")}</div>
        <div className="obody">
          <div className="opt-tabs">
            {tabIds.map((it) => (
              <div
                key={it.id}
                className={`opt-tab${tab === it.id ? " active" : ""}`}
                onClick={() => setTab(it.id)}
              >
                {t(it.key)}
              </div>
            ))}
          </div>
          <div className="opt-pane">
            {tab === "general" && (
              <div className="opt-group">
                <h3>{t("opt.general")}</h3>
                <div className="field">
                  <label>{t("opt.defaultDir")}</label>
                  <div className="row">
                    <input
                      type="text"
                      value={s.downloadDir}
                      onChange={(e) => patch({ downloadDir: e.target.value })}
                    />
                    <button className="btn" onClick={pickFolder}>
                      {t("common.browse")}
                    </button>
                  </div>
                  <span className="hint">{t("opt.defaultDirHint")}</span>
                </div>
                <div className="field">
                  <label className="checkbox">
                    <input
                      type="checkbox"
                      checked={s.categorize}
                      onChange={(e) => patch({ categorize: e.target.checked })}
                    />
                    {t("opt.categorize")}
                  </label>
                </div>

                <h3 style={{ marginTop: 18 }}>{t("opt.startup")}</h3>
                <div className="field">
                  <label className="checkbox">
                    <input
                      type="checkbox"
                      checked={s.autoStart}
                      onChange={(e) => patch({ autoStart: e.target.checked })}
                    />
                    {t("opt.autoStart")}
                  </label>
                </div>
                <div className="field">
                  <label
                    className="checkbox"
                    style={{ opacity: s.autoStart ? 1 : 0.45 }}
                  >
                    <input
                      type="checkbox"
                      disabled={!s.autoStart}
                      checked={s.startMinimized}
                      onChange={(e) =>
                        patch({ startMinimized: e.target.checked })
                      }
                    />
                    {t("opt.startMinimized")}
                  </label>
                </div>

                <h3 style={{ marginTop: 18 }}>{t("opt.update")}</h3>
                <div className="field">
                  <label className="checkbox">
                    <input
                      type="checkbox"
                      checked={s.autoCheckUpdate}
                      onChange={(e) =>
                        patch({ autoCheckUpdate: e.target.checked })
                      }
                    />
                    {t("opt.autoCheckUpdate")}
                  </label>
                </div>
                <div className="field">
                  <div className="row">
                    <button
                      className="btn"
                      onClick={checkUpdate}
                      disabled={busy === "update"}
                    >
                      {busy === "update"
                        ? t("opt.checking")
                        : t("opt.checkNow")}
                    </button>
                    <span className="hint" style={{ flex: 1 }}>
                      {t("opt.currentVersion", { v: appVersion })}
                      {updateMsg}
                    </span>
                  </div>
                </div>
              </div>
            )}

            {tab === "save" && (
              <div className="opt-group">
                <h3>{t("opt.catDirs")}</h3>
                <span className="hint">{t("opt.catDirsHint")}</span>
                <div style={{ height: 12 }} />
                {[
                  "General",
                  "Compressed",
                  "Documents",
                  "Music",
                  "Video",
                  "Programs",
                ].map((c) => (
                  <div className="field" key={c}>
                    <label>{catName(c)}</label>
                    <div className="row">
                      <input
                        type="text"
                        placeholder={t("opt.catDirPlaceholder", {
                          dir: `${s.downloadDir}\\${c}`,
                        })}
                        value={s.categoryDirs?.[c] ?? ""}
                        onChange={(e) =>
                          patch({
                            categoryDirs: {
                              ...s.categoryDirs,
                              [c]: e.target.value,
                            },
                          })
                        }
                      />
                    </div>
                  </div>
                ))}
              </div>
            )}

            {tab === "connection" && (
              <div className="opt-group">
                <h3>{t("opt.connection")}</h3>
                <div className="field">
                  <label>{t("opt.maxConcurrent")}</label>
                  <input
                    type="number"
                    min={1}
                    max={20}
                    value={s.maxConcurrent}
                    onChange={(e) =>
                      patch({ maxConcurrent: Number(e.target.value) })
                    }
                  />
                  <span className="hint">{t("opt.maxConcurrentHint")}</span>
                </div>
                <div className="field">
                  <label>{t("opt.connections")}</label>
                  <input
                    type="number"
                    min={1}
                    max={32}
                    value={s.connections}
                    onChange={(e) =>
                      patch({ connections: Number(e.target.value) })
                    }
                  />
                  <span className="hint">{t("opt.connectionsHint")}</span>
                </div>
                <div className="field">
                  <label className="checkbox">
                    <input
                      type="checkbox"
                      checked={s.adaptiveConnections}
                      onChange={(e) =>
                        patch({ adaptiveConnections: e.target.checked })
                      }
                    />
                    {t("opt.adaptive")}
                  </label>
                </div>
                <div className="field">
                  <label>{t("opt.speedLimit")}</label>
                  <input
                    type="number"
                    min={0}
                    value={Math.round(s.speedLimit / 1024)}
                    onChange={(e) =>
                      patch({
                        speedLimit: Math.max(0, Number(e.target.value)) * 1024,
                      })
                    }
                  />
                </div>
              </div>
            )}

            {tab === "proxy" && (
              <div className="opt-group">
                <h3>{t("opt.proxy")}</h3>
                <div className="field">
                  <label>{t("opt.proxyMode")}</label>
                  <select
                    value={s.proxy.mode}
                    onChange={(e) =>
                      proxyPatch({ mode: e.target.value as any })
                    }
                  >
                    <option value="system">{t("proxy.system")}</option>
                    <option value="none">{t("proxy.none")}</option>
                    <option value="custom">{t("proxy.custom")}</option>
                  </select>
                </div>
                {s.proxy.mode === "custom" && (
                  <>
                    <div className="field">
                      <label>{t("opt.proxyUrl")}</label>
                      <input
                        type="text"
                        placeholder="http://127.0.0.1:7890 / socks5://127.0.0.1:1080"
                        value={s.proxy.url}
                        onChange={(e) => proxyPatch({ url: e.target.value })}
                      />
                      <span className="hint">{t("opt.proxyUrlHint")}</span>
                    </div>
                    <div className="field">
                      <label>{t("opt.proxyAuth")}</label>
                      <div className="row">
                        <input
                          type="text"
                          placeholder={t("add.username")}
                          value={s.proxy.username}
                          onChange={(e) =>
                            proxyPatch({ username: e.target.value })
                          }
                        />
                        <input
                          type="password"
                          placeholder={t("add.password")}
                          value={s.proxy.password}
                          onChange={(e) =>
                            proxyPatch({ password: e.target.value })
                          }
                        />
                      </div>
                    </div>
                  </>
                )}
              </div>
            )}

            {tab === "browser" && (
              <>
                <div className="opt-group">
                  <h3>{t("opt.takeover")}</h3>
                  <div className="field">
                    <label className="checkbox">
                      <input
                        type="checkbox"
                        checked={s.takeoverEnabled}
                        onChange={(e) =>
                          patch({ takeoverEnabled: e.target.checked })
                        }
                      />
                      {t("opt.takeoverEnable")}
                    </label>
                  </div>
                  {s.takeoverEnabled && (
                    <>
                      <div className="field">
                        <label>{t("opt.takeoverPort")}</label>
                        <input
                          type="number"
                          value={s.takeoverPort}
                          onChange={(e) =>
                            patch({ takeoverPort: Number(e.target.value) })
                          }
                        />
                      </div>
                      <div className="field">
                        <label>{t("opt.takeoverOnDownload")}</label>
                        <select
                          value={s.takeoverAction}
                          onChange={(e) =>
                            patch({ takeoverAction: e.target.value as any })
                          }
                        >
                          <option value="dialog">
                            {t("opt.takeoverDialog")}
                          </option>
                          <option value="auto">{t("opt.takeoverAuto")}</option>
                        </select>
                      </div>
                    </>
                  )}
                </div>

                <div className="opt-group">
                  <h3>{t("opt.extSection")}</h3>
                  <ManualInstall />
                  <div className="field" style={{ marginTop: 12 }}>
                    <label className="checkbox">
                      <input
                        type="checkbox"
                        checked={!s.extPromptIgnored}
                        onChange={(e) =>
                          patch({ extPromptIgnored: !e.target.checked })
                        }
                      />
                      {t("opt.extRemind")}
                    </label>
                  </div>
                  <div className="note">{t("opt.extNote")}</div>
                </div>
              </>
            )}

            {tab === "language" && (
              <div className="opt-group">
                <h3>{t("opt.language")}</h3>
                <div className="field">
                  <label>{t("opt.languageLabel")}</label>
                  <select
                    value={s.language === "en" ? "en" : "zh"}
                    onChange={(e) => pickLanguage(e.target.value as Lang)}
                  >
                    <option value="zh">{t("opt.langZh")}</option>
                    <option value="en">{t("opt.langEn")}</option>
                  </select>
                  <span className="hint">{t("opt.languageHint")}</span>
                </div>
              </div>
            )}

            {error && (
              <div className="status-text err" style={{ marginTop: 8 }}>
                {error}
              </div>
            )}
          </div>
        </div>
        <div className="actions">
          <button className="btn" onClick={cancel}>
            {t("common.cancel")}
          </button>
          <button className="btn primary" onClick={save}>
            {t("common.save")}
          </button>
        </div>
      </div>
    </div>
  );
}

function catName(c: string): string {
  const key = `cat.${c}`;
  const label = t(key);
  return label === key ? c : label;
}
