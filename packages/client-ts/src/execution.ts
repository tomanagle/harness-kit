import type { Command, Lease, RemoteProtocol } from "./remote.gen.ts";

export interface ExecutionHandler {
  /** Persist the fencing epoch before commands can be admitted. */
  acquired?(lease: Lease): Promise<void>;
  /** Persist/admit the command before resolving. Use command.id as a dedupe ID.
   * Do not await an entire prompt here: allow cancel/approval commands to flow. */
  handle(command: Command, signal: AbortSignal): Promise<Record<string, unknown>>;
  /** Stop local execution when ownership becomes uncertain. */
  lost(reason: unknown): void | Promise<void>;
}
export interface ExecutionOwner {
  readonly lease: Lease;
  stop(): Promise<void>;
}
/** Host execution is explicit. The server coordinates; only the injected host
 * knows how to admit a prompt, persist native state, cancel, or answer approval. */
export async function serveExecution(
  protocol: RemoteProtocol,
  sessionId: string,
  expectedEpoch: number,
  handler: ExecutionHandler,
): Promise<ExecutionOwner> {
  let lease = await protocol.acquireLease({ sessionId, expectedEpoch });
  if (lease.sessionId !== sessionId || lease.epoch < 1) throw new Error("Invalid execution lease");
  try {
    await handler.acquired?.(lease);
  } catch (error) {
    await protocol.releaseLease({ sessionId, epoch: lease.epoch }).catch(() => {});
    throw error;
  }
  const controller = new AbortController();
  let deadline: ReturnType<typeof setTimeout> | undefined;
  let failure: unknown;
  const lose = (error: unknown) => {
    failure ??= error;
    controller.abort(error);
  };
  const armDeadline = () => {
    clearTimeout(deadline);
    const remaining = Date.parse(lease.expiresAt) - Date.now() - 2000;
    if (!Number.isFinite(remaining) || remaining <= 0) {
      lose(new Error("Execution lease expired"));
      return;
    }
    deadline = setTimeout(
      () => lose(new Error("Execution lease renewal timed out")),
      Math.min(remaining, 28000),
    );
  };
  armDeadline();
  const heartbeat = (async () => {
    try {
      while (!controller.signal.aborted) {
        await delay(8000, controller.signal);
        if (controller.signal.aborted) break;
        const renewed = await protocol.renewLease(
          { sessionId, epoch: lease.epoch },
          { signal: controller.signal },
        );
        if (
          renewed.sessionId !== sessionId ||
          renewed.epoch !== lease.epoch ||
          renewed.deviceId !== lease.deviceId
        )
          throw new Error("Execution lease changed");
        lease = renewed;
        armDeadline();
      }
    } catch (error) {
      if (!controller.signal.aborted) lose(error);
    }
  })();
  const worker = (async () => {
    try {
      while (!controller.signal.aborted) {
        const page = await protocol.claimCommand(
          { sessionId, epoch: lease.epoch },
          { signal: controller.signal },
        );
        for (const command of page.commands) {
          if (command.sessionId !== sessionId || command.epoch !== lease.epoch)
            throw new Error("Command scope mismatch");
          let result: Record<string, unknown>,
            status: "completed" | "failed" = "completed";
          try {
            result = await handler.handle(command, controller.signal);
          } catch {
            status = "failed";
            result = { code: "host_command_failed" };
          }
          if (controller.signal.aborted) break;
          // Retry only the acknowledgement, never the external action. If it stays
          // uncertain, stop ownership and let the user reconcile its outcome.
          const input = { id: command.id, sessionId, epoch: lease.epoch, status, result };
          try {
            await protocol.completeCommand(input, { signal: controller.signal });
          } catch {
            await protocol.completeCommand(input, { signal: controller.signal });
          }
        }
        await delay(250, controller.signal);
      }
    } catch (error) {
      if (!controller.signal.aborted) lose(error);
    } finally {
      controller.abort();
      clearTimeout(deadline);
      try {
        await handler.lost(failure ?? new Error("Execution owner stopped"));
      } catch (error) {
        failure ??= error;
      }
    }
  })();
  return {
    get lease() {
      return { ...lease };
    },
    async stop() {
      controller.abort();
      await Promise.all([worker, heartbeat]);
      if (!failure) await protocol.releaseLease({ sessionId, epoch: lease.epoch });
    },
  };
}
function delay(ms: number, signal: AbortSignal): Promise<void> {
  if (signal.aborted) return Promise.resolve();
  return new Promise((resolve) => {
    const done = () => {
      clearTimeout(timer);
      signal.removeEventListener("abort", done);
      resolve();
    };
    const timer = setTimeout(done, ms);
    signal.addEventListener("abort", done, { once: true });
  });
}
