import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import vm from "node:vm";
import { webcrypto } from "node:crypto";

async function fixture(browser, mode) {
  const calls = [];
  const saved = {};
  const event = () => ({ addListener() {} });
  const api = {
    storage: {
      local: {
        async get(keys) {
          return keys === null ? { ...saved } : { enabled: true, port: 9614 };
        },
        async set(values) {
          Object.assign(saved, values);
        },
        async remove(key) {
          delete saved[key];
        },
      },
    },
    downloads: Object.fromEntries(
      ["pause", "cancel", "erase", "resume"].map((method) => [
        method,
        async () => {
          calls.push(method);
        },
      ]),
    ),
    cookies: {
      async getAll() {
        return [];
      },
    },
    runtime: { onStartup: event(), onInstalled: event(), onMessage: event() },
    alarms: { onAlarm: event(), create() {} },
    action: { async setBadgeText() {}, async setBadgeBackgroundColor() {} },
  };
  api.downloads.onCreated = event();
  const context = vm.createContext({
    chrome: api,
    navigator: { userAgent: "test" },
    crypto: webcrypto,
    AbortSignal,
    async fetch(url, options) {
      if (url.endsWith("/ping")) {
        if (mode === "absent") throw Error("offline");
        return {
          ok: true,
          json: async () => ({ app: "Better Download Manager", protocol: "2" }),
        };
      }
      if (url.includes("/download/status")) {
        if (mode === "unknown") throw Error("offline");
        return {
          ok: true,
          json: async () => ({
            accepted: mode !== "rejected",
            state: mode === "rejected" ? "failed" : "task",
          }),
        };
      }
      calls.push("post");
      if (mode === "lost" || mode === "unknown") throw Error("response lost");
      return {
        ok: mode !== "rejected",
        json: async () => ({
          accepted: mode !== "rejected",
          requestId: JSON.parse(options.body).requestId,
        }),
      };
    },
  });
  vm.runInContext(
    await fs.readFile(
      new URL(`../../extensions/${browser}/background.js`, import.meta.url),
      "utf8",
    ),
    context,
  );
  await new Promise((resolve) => setImmediate(resolve));
  await vm.runInContext(
    'intercept({id:7,url:"https://example.com/file",filename:"file.bin"})',
    context,
  );
  return { calls, saved };
}
for (const browser of ["chromium", "firefox"]) {
  test(`${browser}: offline app keeps original download running`, async () => {
    const { calls } = await fixture(browser, "absent");
    assert.deepEqual(calls, []);
  });
  test(`${browser}: cancellation follows durable acknowledgement`, async () => {
    const { calls, saved } = await fixture(browser, "accepted");
    assert.deepEqual(calls, ["pause", "post", "cancel", "erase"]);
    assert.equal(Object.keys(saved).length, 0);
  });
  test(`${browser}: lost response is reconciled without duplicate download`, async () => {
    const { calls } = await fixture(browser, "lost");
    assert.deepEqual(calls, ["pause", "post", "cancel", "erase"]);
  });
  test(`${browser}: rejected receipt resumes the original`, async () => {
    const { calls } = await fixture(browser, "rejected");
    assert.deepEqual(calls, ["pause", "post", "resume"]);
  });
  test(`${browser}: unknown outcome retains the paused original and receipt`, async () => {
    const { calls, saved } = await fixture(browser, "unknown");
    assert.deepEqual(calls, ["pause", "post"]);
    assert.equal(Object.keys(saved).length, 1);
  });
}
