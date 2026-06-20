import { useCallback, useEffect, useMemo, useState } from "react";
import "./styles.css";
import {
  api,
  onEvent,
  EVT_TASK_UPDATE,
  EVT_TASK_REMOVED,
  type TaskInfo,
  type Settings,
} from "./api";
import MenuBar from "./components/MenuBar";
import Toolbar from "./components/Toolbar";
import Sidebar, { type Filter } from "./components/Sidebar";
import TaskTable from "./components/TaskTable";
import OptionsDialog from "./components/OptionsDialog";
import ProgressDialog from "./components/ProgressDialog";
import ExtPromptDialog from "./components/ExtPromptDialog";
import UpdateDialog, { type UpdateInfo } from "./components/UpdateDialog";
import { formatSpeed } from "./format";
import { t, setLang, useLang } from "./i18n";

const APP_VERSION = "1.1.2";

export default function App() {
  useLang(); // re-render the whole tree when the language changes
  const [tasks, setTasks] = useState<Map<string, TaskInfo>>(new Map());
  const [categories, setCategories] = useState<string[]>([]);
  const [filter, setFilter] = useState<Filter>({ kind: "all" });
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [showOptions, setShowOptions] = useState(false);
  const [detailsId, setDetailsId] = useState<string | null>(null);
  const [settings, setSettings] = useState<Settings | null>(null);
  const [extPrompt, setExtPrompt] = useState(false);
  const [showAbout, setShowAbout] = useState(false);
  const [update, setUpdate] = useState<UpdateInfo | null>(null);

  const upsert = useCallback((t: TaskInfo) => {
    setTasks((prev) => new Map(prev).set(t.id, t));
  }, []);

  const reload = useCallback(async () => {
    const list = await api.listTasks();
    const m = new Map<string, TaskInfo>();
    for (const t of list ?? []) m.set(t.id, t);
    setTasks(m);
  }, []);

  useEffect(() => {
    reload();
    api.categories().then((c) => setCategories(c ?? []));
    api.getSettings().then(async (cfg) => {
      setLang(cfg.language);
      setSettings(cfg);
      // On startup, offer the one-time manual extension install (unless ignored).
      if (cfg.takeoverEnabled && !cfg.extPromptIgnored) {
        const configured = await api.browserExtensionConfigured().catch(() => false);
        if (!configured) setExtPrompt(true);
      }
      // Auto-check for updates (silent unless a newer version exists).
      if (cfg.autoCheckUpdate) {
        api.checkForUpdates()
          .then((r) => { if (r.hasUpdate) setUpdate(r as UpdateInfo); })
          .catch(() => {});
      }
    });

    // Manual "立即检查更新" in Options dispatches this when an update is found.
    const onUpd = (e: Event) => setUpdate((e as CustomEvent).detail as UpdateInfo);
    window.addEventListener("bdm:update", onUpd);

    const offUpdate = onEvent<TaskInfo>(EVT_TASK_UPDATE, upsert);
    const offRemoved = onEvent<string>(EVT_TASK_REMOVED, (id) =>
      setTasks((prev) => {
        const next = new Map(prev);
        next.delete(id);
        return next;
      })
    );
    return () => {
      offUpdate();
      offRemoved();
      window.removeEventListener("bdm:update", onUpd);
    };
  }, [reload, upsert]);

  const allTasks = useMemo(() => Array.from(tasks.values()), [tasks]);

  const filtered = useMemo(
    () =>
      allTasks.filter((t) => {
        switch (filter.kind) {
          case "all": return true;
          case "active": return t.status !== "completed";
          case "done": return t.status === "completed";
          case "category": return t.category === filter.name;
        }
      }),
    [allTasks, filter]
  );

  const selected = selectedId ? tasks.get(selectedId) ?? null : null;
  const detailsTask = detailsId ? tasks.get(detailsId) ?? null : null;
  const totalSpeed = allTasks.reduce((sum, t) => sum + (t.status === "downloading" ? t.speed : 0), 0);
  const activeCount = allTasks.filter((t) => t.status === "downloading").length;
  const hasCompleted = allTasks.some((t) => t.status === "completed");

  const canResume = !!selected && ["paused", "error", "queued"].includes(selected.status);
  const canStop = !!selected && ["downloading", "connecting", "queued"].includes(selected.status);

  const openAdd = async () => {
    let url = "";
    try {
      const text = await navigator.clipboard.readText();
      if (/^https?:\/\//i.test(text.trim())) url = text.trim();
    } catch {
      /* clipboard may be unavailable; ignore */
    }
    api.showAddWindow({ url });
  };

  const deleteCompleted = () => {
    for (const t of allTasks) if (t.status === "completed") api.removeTask(t.id, false);
  };

  const copyUrl = (url: string) => {
    navigator.clipboard?.writeText(url).catch(() => {});
  };

  const ignoreExt = async () => {
    if (settings) {
      const next = { ...settings, extPromptIgnored: true } as Settings;
      await api.saveSettings(next);
      setSettings(next);
    }
    setExtPrompt(false);
  };

  const checkUpdateNow = async () => {
    try {
      const r = await api.checkForUpdates();
      if (r.hasUpdate) setUpdate(r as UpdateInfo);
      else alert(t("update.latestAlready", { v: r.current }));
    } catch (e: any) {
      alert(t("update.checkFailed", { err: String(e?.message ?? e) }));
    }
  };

  return (
    <div className="app">
      <MenuBar
        menus={[
          {
            title: t("menu.task"),
            items: [
              { label: t("menu.addUrl"), shortcut: "Ctrl+N", onClick: openAdd },
              { label: "", separator: true },
              { label: t("menu.startAll"), onClick: () => api.startAll() },
              { label: t("menu.pauseAll"), onClick: () => api.pauseAll() },
              { label: "", separator: true },
              { label: t("menu.deleteCompleted"), onClick: deleteCompleted, disabled: !hasCompleted },
            ],
          },
          {
            title: t("menu.view"),
            items: [
              { label: t("menu.refresh"), onClick: () => reload() },
              { label: t("menu.options"), onClick: () => setShowOptions(true) },
            ],
          },
          {
            title: t("menu.help"),
            items: [
              { label: t("menu.checkUpdate"), onClick: checkUpdateNow },
              { label: t("menu.about"), onClick: () => setShowAbout(true) },
            ],
          },
        ]}
      />

      <Toolbar
        canResume={canResume}
        canStop={canStop}
        hasSelection={!!selected}
        hasCompleted={hasCompleted}
        onAdd={openAdd}
        onResume={() => selected && api.startTask(selected.id)}
        onStop={() => selected && api.pauseTask(selected.id)}
        onStartAll={() => api.startAll()}
        onStopAll={() => api.pauseAll()}
        onDelete={() => selected && api.removeTask(selected.id, false)}
        onDeleteCompleted={deleteCompleted}
        onOpenFolder={() => selected && api.openFolder(selected.id)}
        onOptions={() => setShowOptions(true)}
      />

      <div className="body">
        <Sidebar categories={categories} tasks={allTasks} filter={filter} onSelect={setFilter} />
        <TaskTable
          tasks={filtered}
          selectedId={selectedId}
          onSelect={setSelectedId}
          onResume={(id) => api.startTask(id)}
          onPause={(id) => api.pauseTask(id)}
          onRemove={(id, del) => api.removeTask(id, del)}
          onOpenFile={(id) => api.openFile(id)}
          onOpenFolder={(id) => api.openFolder(id)}
          onDetails={(id) => setDetailsId(id)}
          onCopyUrl={copyUrl}
        />
      </div>

      <div className="statusbar">
        <span>{t("status.total", { n: allTasks.length })}</span>
        <span>{t("status.active", { n: activeCount })}</span>
        <span className="sb-grow" />
        {totalSpeed > 0 && <span className="sb-speed">↓ {formatSpeed(totalSpeed)}</span>}
      </div>

      {showOptions && (
        <OptionsDialog onClose={() => setShowOptions(false)} onSaved={(s) => setSettings(s)} />
      )}

      {detailsTask && (
        <ProgressDialog
          task={detailsTask}
          onResume={(id) => api.startTask(id)}
          onPause={(id) => api.pauseTask(id)}
          onClose={() => setDetailsId(null)}
        />
      )}

      {extPrompt && (
        <ExtPromptDialog onLater={() => setExtPrompt(false)} onIgnore={ignoreExt} />
      )}

      {update && <UpdateDialog info={update} onClose={() => setUpdate(null)} />}

      {showAbout && (
        <div className="overlay" onMouseDown={() => setShowAbout(false)}>
          <div className="dialog" style={{ width: 380 }} onMouseDown={(e) => e.stopPropagation()}>
            <div className="titlebar">{t("about.title")}</div>
            <div className="content" style={{ textAlign: "center", lineHeight: 1.8 }}>
              <div style={{ fontSize: 16, fontWeight: 600 }}>Better Download Manager</div>
              <div>{t("about.version", { v: APP_VERSION })}</div>
              <div style={{ color: "var(--muted)", fontSize: 12 }}>{t("about.tagline")}</div>
            </div>
            <div className="actions">
              <button className="btn primary" onClick={() => setShowAbout(false)}>{t("common.ok")}</button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
