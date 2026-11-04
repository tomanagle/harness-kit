// Electron main process: owns the harness-kit sidecar and exposes a small,
// typed IPC API to the renderer (the shared dashboard in ../ui). The
// renderer never sees raw JSON-RPC, so web content can only do what this
// API allows.
//
//   dashboard (../ui/app.js) -> preload -> ipcMain -> @tomanagle/harness-kit -> stdio -> harness-kit
//
// The demo host tools (../ui/demo-tools.js) are registered here, so their
// handlers run in this process.

import { app, BrowserWindow, ipcMain } from "electron";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createHarnessKit } from "@tomanagle/harness-kit";
import { stdio } from "@tomanagle/harness-kit/node";
import { makeApi } from "../ui/api.js";
import { demoTools } from "../ui/demo-tools.js";

const here = path.dirname(fileURLToPath(import.meta.url));
const smoke = Boolean(process.env.HARNESS_KIT_SMOKE);
// Sessions start in this directory (the repo, when run with make).
const cwd = process.env.HARNESS_KIT_EXAMPLE_CWD || process.cwd();

// In development the binary is built into ../../core/bin. When packaging, ship
// it as an extra resource (electron-builder "extraResources") per platform.
function sidecarPath() {
  const exe = process.platform === "win32" ? "harness-kit.exe" : "harness-kit";
  return app.isPackaged
    ? path.join(process.resourcesPath, exe)
    : path.join(here, "..", "..", "core", "bin", exe);
}

let kit;
let win;
let storeDir;

async function start() {
  // Session history survives app restarts; the smoke test uses a throwaway store.
  storeDir = smoke
    ? fs.mkdtempSync(path.join(os.tmpdir(), "hk-electron-smoke-"))
    : path.join(app.getPath("userData"), "sessions");
  kit = await createHarnessKit({
    transport: stdio({
      command: sidecarPath(),
      args: ["serve", "--fake-runtime", "--kit-mcp", "--store", storeDir],
      stderr: "inherit",
    }),
    clientInfo: { name: "electron-example", version: app.getVersion() },
  });
  await kit.tools.register(demoTools);
  // Fan all events out to the renderer; it filters by session.
  kit.onEvent((event) => win?.webContents.send("harness-kit:event", event));
  kit.onClose((err) => win?.webContents.send("harness-kit:closed", err?.message ?? null));
  // One IPC handler per API method: this object is the renderer's whole reach.
  const api = makeApi(kit, { cwd });
  for (const [name, fn] of Object.entries(api)) {
    ipcMain.handle(`harness-kit:${name}`, (_e, ...args) => fn(...args));
  }
}

app.whenReady().then(async () => {
  await start();
  win = new BrowserWindow({
    width: 1100,
    height: 760,
    show: !smoke,
    webPreferences: {
      preload: path.join(here, "preload.cjs"),
      contextIsolation: true,
      sandbox: true,
      nodeIntegration: false,
    },
  });
  await win.loadFile(path.join(here, "index.html"));
  if (smoke) await smokeTest();
});

// HARNESS_KIT_SMOKE=1 (`make example-electron-smoke`): drive the real
// dashboard in a hidden window, the way a person would, then quit.
async function smokeTest() {
  try {
    const r = await win.webContents.executeJavaScript(`(async () => {
      const $ = (s) => document.querySelector(s);
      const wait = async (cond, what) => {
        for (const t0 = Date.now(); !cond(); await new Promise((r) => setTimeout(r, 25)))
          if (Date.now() - t0 > 20000) throw new Error("timed out waiting for " + what);
      };
      await wait(() => document.body.dataset.ready, "the dashboard");
      if (document.body.dataset.ready !== "1") throw new Error("dashboard failed: " + $("#transcript").innerText);
      const texts = (s) => [...document.querySelectorAll(s)].map((e) => e.textContent);
      const runtimes = texts("#runtimes .name"), tools = texts("#tools .name"), mcp = $("#mcp-note").textContent;
      $("#new-runtime").value = "fake";
      $("#new-session").click();
      await wait(() => !$("#prompt").disabled, "a session");
      $("#prompt").value = '[call order_status {"id":"A7"}] [call refund {"id":"A7"}] smoke';
      $("#composer").requestSubmit();
      await wait(() => !$("#approval").hidden, "the approval bar");
      const approval = $(".approval-text").textContent;
      $('#approval [data-outcome="allow"]').click();
      await wait(() => $(".turn-end"), "the end of the turn");
      await wait(() => texts("#sessions li.session .name").some((t) => t.includes("smoke")), "the session list");
      const transcript = $("#transcript").innerText;
      $("#rename").click();
      $("#rename-input").value = "Renamed by the smoke test";
      $("#rename-input").dispatchEvent(new KeyboardEvent("keydown", { key: "Enter" }));
      await wait(() => texts("#sessions li.session .name").includes("Renamed by the smoke test"), "the renamed session");
      return { runtimes, tools, mcp, approval, transcript, sessions: texts("#sessions li.session .name"), title: $("#session-title").textContent };
    })()`);
    const checks = {
      "runtimes listed": r.runtimes.includes("fake") && r.runtimes.includes("codex"),
      "host tools listed": r.tools.join() === "order_status,refund",
      "MCP panel filled": r.mcp.length > 0,
      "refund asked for approval": r.approval.includes("refund"),
      "order_status ran": r.transcript.includes('"status":"shipped"'),
      "refund ran after approval": r.transcript.includes("Order A7 refunded"),
      "turn completed": r.transcript.includes("completed"),
      "session in the list": r.sessions.length > 0,
      "session renamed": r.title === "Renamed by the smoke test",
    };
    const failed = Object.entries(checks)
      .filter(([, ok]) => !ok)
      .map(([k]) => k);
    console.log(failed.length ? `SMOKE FAILED: ${failed.join(", ")}` : "SMOKE OK");
    for (const [k, ok] of Object.entries(checks)) console.log(`  ${ok ? "✓" : "✗"} ${k}`);
    console.log(`  runtimes: ${r.runtimes.join(", ")} | tools: ${r.tools.join(", ")}`);
    console.log(`  MCP: ${r.mcp}`);
    process.exitCode = failed.length ? 1 : 0;
    if (process.env.HARNESS_KIT_SMOKE_SCREENSHOT) {
      fs.writeFileSync(
        process.env.HARNESS_KIT_SMOKE_SCREENSHOT,
        (await win.webContents.capturePage()).toPNG(),
      );
      console.log(`  screenshot: ${process.env.HARNESS_KIT_SMOKE_SCREENSHOT}`);
    }
  } catch (e) {
    console.error("SMOKE FAILED", e);
    process.exitCode = 1;
  }
  app.quit();
}

// Closing the kit shuts down every session and runtime process. If the app
// crashes instead, the sidecar sees stdin close and cleans up by itself.
let quitting = false;
app.on("before-quit", (e) => {
  if (quitting || !kit) return;
  e.preventDefault();
  quitting = true;
  kit.close().finally(() => {
    if (smoke && storeDir) fs.rmSync(storeDir, { recursive: true, force: true });
    app.quit();
  });
});
app.on("window-all-closed", () => app.quit());
