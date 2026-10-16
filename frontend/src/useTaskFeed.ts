import { useCallback, useEffect, useRef, useState } from "react";
import {
  api,
  onEvent,
  EVT_TASK_UPDATE,
  EVT_TASK_REMOVED,
  type TaskInfo,
} from "./api";
import { TaskState } from "./taskState";

export function useTaskFeed() {
  const state = useRef(new TaskState<TaskInfo>());
  const [tasks, setTasks] = useState(state.current.records);
  const reload = useCallback(async () => {
    const list = await api.listTasks();
    setTasks(state.current.apply(list ?? []));
  }, []);
  useEffect(() => {
    let frame = 0;
    let pending: TaskInfo[] = [];
    const offUpdate = onEvent<TaskInfo>(EVT_TASK_UPDATE, (task) => {
      pending.push(task);
      if (!frame)
        frame = requestAnimationFrame(() => {
          frame = 0;
          setTasks(state.current.apply(pending));
          pending = [];
        });
    });
    const offRemoved = onEvent<string>(EVT_TASK_REMOVED, (id) => {
      setTasks(state.current.apply([], [id]));
    });
    // Subscribe before fetching the initial snapshot.
    reload().catch((error) =>
      window.dispatchEvent(
        new CustomEvent("bdm:error", { detail: String(error) }),
      ),
    );
    return () => {
      offUpdate();
      offRemoved();
      cancelAnimationFrame(frame);
    };
  }, [reload]);
  return { tasks, reload };
}
