// Sandboxed preload: exposes exactly the dashboard API (see ../ui/api.js)
// as window.harnessKit. Each call is an IPC round trip to the main process.
const { contextBridge, ipcRenderer } = require("electron");

const call =
  (name) =>
  (...args) =>
    ipcRenderer.invoke(`harness-kit:${name}`, ...args);

contextBridge.exposeInMainWorld("harnessKit", {
  overview: call("overview"),
  listSessions: call("listSessions"),
  createSession: call("createSession"),
  renameSession: call("renameSession"),
  openSession: call("openSession"),
  history: call("history"),
  gitStatus: call("gitStatus"),
  gitDiff: call("gitDiff"),
  gitBranches: call("gitBranches"),
  gitLog: call("gitLog"),
  gitStage: call("gitStage"),
  gitUnstage: call("gitUnstage"),
  gitCommit: call("gitCommit"),
  gitCheckout: call("gitCheckout"),
  gitFetch: call("gitFetch"),
  gitPull: call("gitPull"),
  gitPush: call("gitPush"),
  prompt: call("prompt"),
  cancel: call("cancel"),
  approve: call("approve"),
  onEvent: (listener) => {
    const handler = (_e, event) => listener(event);
    ipcRenderer.on("harness-kit:event", handler);
    return () => ipcRenderer.removeListener("harness-kit:event", handler);
  },
});
