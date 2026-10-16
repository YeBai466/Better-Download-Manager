import test from "node:test";
import assert from "node:assert/strict";
import { TaskState } from "../src/taskState.ts";

test("late initial snapshot preserves a newer progress event", () => {
  const state = new TaskState();
  state.apply([{ id: "one", revision: 3, status: "completed" }]);
  state.apply([{ id: "one", revision: 1, status: "downloading" }]);
  assert.equal(state.records.get("one").status, "completed");
});
test("removed task stays removed after late progress and reload", () => {
  const state = new TaskState();
  state.apply([{ id: "one", revision: 1 }]);
  state.apply([], ["one"]);
  state.apply([{ id: "one", revision: 10 }]);
  assert.equal(state.records.size, 0);
});
test("ten thousand history records retain identity when an active task changes", () => {
  const state = new TaskState();
  const original = Array.from({ length: 10000 }, (_, i) => ({
    id: String(i),
    revision: 1,
    status: "completed",
  }));
  state.apply(original);
  const before = state.records;
  state.apply([{ id: "9999", revision: 2, status: "downloading" }]);
  assert.notEqual(state.records, before);
  assert.equal(state.records.size, 10000);
  assert.equal(state.records.get("1"), original[1]);
  const unchanged = state.records;
  state.apply([{ id: "9999", revision: 1 }]);
  assert.equal(state.records, unchanged);
});
