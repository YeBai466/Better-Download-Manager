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
import { formatSpeed } from "./format";

export default function App() {
  const [tasks, setTasks] = useState<Map<string, TaskInfo>>(new Map());
  const [categories, setCategories] = useState<string[]>([]);
  const [filter, setFilter] = useState<Filter>({ kind: "all" });
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [showOptions, setShowOptions] = useState(false);
  const [detailsId, setDetailsId] = useState<string | null>(null);
  const [settings, setSettings] = useState<Settings | null>(null);
  const [extPrompt, setExtPrompt] = useState(false);
  const [showAbout, setShowAbout] = useState(false);

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
    api.getSettings().then((cfg) => {
      setSettings(cfg);
      // On startup, offer the one-time manual extension install (unless ignored).
      if (cfg.takeoverEnabled && !cfg.extPromptIgnored) setExtPrompt(true);
    });

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
  const canStop = !!selected && ["downloading", "connecting"].includes(selected.status);

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

  return (
    <div className="app">
      <MenuBar
        menus={[
          {
            title: "任务",
            items: [
              { label: "添加 URL…", shortcut: "Ctrl+N", onClick: openAdd },
              { label: "", separator: true },
              { label: "全部开始", onClick: () => api.startAll() },
              { label: "全部暂停", onClick: () => api.pauseAll() },
              { label: "", separator: true },
              { label: "删除已完成", onClick: deleteCompleted, disabled: !hasCompleted },
            ],
          },
          {
            title: "查看",
            items: [
              { label: "刷新列表", onClick: () => reload() },
              { label: "选项…", onClick: () => setShowOptions(true) },
            ],
          },
          {
            title: "帮助",
            items: [{ label: "关于 B Download Manager", onClick: () => setShowAbout(true) }],
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
        <span>共 {allTasks.length} 个任务</span>
        <span>下载中 {activeCount}</span>
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

      {showAbout && (
        <div className="overlay" onMouseDown={() => setShowAbout(false)}>
          <div className="dialog" style={{ width: 380 }} onMouseDown={(e) => e.stopPropagation()}>
            <div className="titlebar">关于</div>
            <div className="content" style={{ textAlign: "center", lineHeight: 1.8 }}>
              <div style={{ fontSize: 16, fontWeight: 600 }}>B Download Manager</div>
              <div>版本 1.0.0</div>
              <div style={{ color: "var(--muted)", fontSize: 12 }}>Go + React (Wails v3) · 仿 IDM 多线程下载器</div>
            </div>
            <div className="actions">
              <button className="btn primary" onClick={() => setShowAbout(false)}>确定</button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
