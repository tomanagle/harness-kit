// Builds window.harnessKit for the Wails example.
//
//   Sidecar mode (default): the frontend uses the same @tomanagle/harness-kit
//   as Electron; lines go out through the Bridge.Send binding and come back
//   as "harness-kit:rpc" events. The demo tools are registered here, so
//   their handlers run in the webview.
//
//   Direct mode (HARNESS_KIT_MODE=direct): Harness Kit is linked into the Go
//   backend; this file maps the API onto the Direct bindings, and the demo
//   tools are Go functions.
import { createHarnessKit, callbackTransport } from "./vendor/index.js";
import { makeApi } from "./api.js";
import { demoTools } from "./demo-tools.js";

window.harnessKitReady = (async () => {
  const app = window.go?.main;
  if (app?.Direct) {
    const d = app.Direct;
    return {
      overview: () => d.Overview(),
      listSessions: () => d.ListSessions(),
      createSession: (runtime) => d.CreateSession(runtime),
      renameSession: (id, title) => d.RenameSession(id, title),
      openSession: (id) => d.OpenSession(id),
      history: (id) => d.History(id),
      context: (id) => d.Context(id),
      compact: (id) => d.Compact(id),
      gitStatus: (id) => d.GitStatus(id),
      gitDiff: (id, options = {}) => d.GitDiff(id, options),
      gitBranches: (id) => d.GitBranches(id),
      gitLog: (id, options = {}) => d.GitLog(id, options),
      gitStage: (id, paths) => d.GitStage(id, { paths }),
      gitUnstage: (id, paths) => d.GitUnstage(id, { paths }),
      gitCommit: (id, message) => d.GitCommit(id, { message }),
      gitCheckout: (id, options) => d.GitCheckout(id, options),
      gitFetch: (id, options = {}) => d.GitFetch(id, options),
      gitPull: (id, options = {}) => d.GitPull(id, options),
      gitPush: (id, options = {}) => d.GitPush(id, options),
      prompt: (id, text) => d.Prompt(id, text),
      cancel: (id) => d.Cancel(id),
      approve: (approvalId, outcome) => d.RespondApproval(approvalId, outcome),
      onEvent: (listener) => window.runtime.EventsOn("harness-kit:event", listener),
    };
  }
  const kit = await createHarnessKit({
    transport: callbackTransport({
      send: (line) => app.Bridge.Send(line),
      subscribe: (onMessage) => window.runtime.EventsOn("harness-kit:rpc", onMessage),
    }),
    clientInfo: { name: "wails-example" },
  });
  await kit.tools.register(demoTools);
  return { ...makeApi(kit), onEvent: (listener) => kit.onEvent(listener) };
})();
