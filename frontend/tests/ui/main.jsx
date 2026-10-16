import React, { useCallback, useState } from "react";
import { createRoot } from "react-dom/client";
import TaskTable from "../../src/components/TaskTable";
import { setLang } from "../../src/i18n";
import "../../src/styles.css";

const initial = Array.from({ length: 10000 }, (_, i) => ({
  id: String(i),
  revision: 1,
  filename: `file-${String(i).padStart(5, "0")}.bin`,
  url: "https://example.invalid/file",
  savePath: `C:\\Downloads\\file-${i}.bin`,
  category: "General",
  totalSize: 64 << 20,
  resumable: true,
  connections: 8,
  mime: "application/octet-stream",
  status: i < 5 ? "downloading" : "completed",
  error: "",
  downloaded: 32 << 20,
  speed: 1 << 20,
  progress: 0.5,
  etaSeconds: 32,
  segments: [],
  createdAt: new Date(1700000000000 + i * 1000).toISOString(),
}));
function Fixture() {
  const [tasks, setTasks] = useState(initial);
  const [selectedId, setSelected] = useState(null);
  const noop = useCallback(() => {}, []);
  const update = () =>
    setTasks((old) =>
      old.map((task, i) =>
        i < 5
          ? {
              ...task,
              revision: task.revision + 1,
              downloaded: task.downloaded + 1024,
            }
          : task,
      ),
    );
  return (
    <div style={{ height: "100vh", display: "flex", flexDirection: "column" }}>
      <div style={{ padding: 8 }}>
        <strong>10,000 tasks</strong>{" "}
        <button onClick={update}>Update progress</button>{" "}
        <button onClick={() => setLang("en")}>English</button>{" "}
        <button onClick={() => setTasks([])}>Clear</button>{" "}
        <button onClick={() => setTasks(initial)}>Restore</button>{" "}
        <span id="selected">Selected: {selectedId ?? "none"}</span>
      </div>
      <TaskTable
        tasks={tasks}
        selectedId={selectedId}
        onSelect={setSelected}
        onResume={noop}
        onPause={noop}
        onRemove={noop}
        onOpenFile={noop}
        onOpenFolder={noop}
        onDetails={noop}
        onCopyUrl={noop}
      />
    </div>
  );
}
createRoot(document.getElementById("root")).render(<Fixture />);
