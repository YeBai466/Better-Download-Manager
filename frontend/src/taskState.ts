export interface VersionedTask {
  id: string;
  revision?: number;
}

// A removed ID stays terminal for this window's session. Late snapshots and
// progress packets cannot resurrect it or replace a more recent revision.
export class TaskState<T extends VersionedTask> {
  records = new Map<string, T>();
  private removed = new Set<string>();

  apply(updates: T[], removed: string[] = []): Map<string, T> {
    let next = this.records;
    const writable = () => {
      if (next === this.records) next = new Map(next);
    };
    for (const id of removed) {
      this.removed.add(id);
      if (next.has(id)) {
        writable();
        next.delete(id);
      }
    }
    for (const task of updates) {
      if (this.removed.has(task.id)) continue;
      const old = next.get(task.id);
      if (old && (old.revision ?? 0) >= (task.revision ?? 0)) continue;
      writable();
      next.set(task.id, task);
    }
    this.records = next;
    return next;
  }
}
