// Compile-only API assertions. These functions are never executed.
import type { HarnessEvent, HarnessKit, SettingsPatch } from "../../src/index.ts";

export async function generatedMethods(kit: HarnessKit): Promise<void> {
  const { session } = await kit.protocol.sessionCreate({
    settings: { session: { runtime: "fake" } },
  });
  const id: string = session.sessionId;
  const page = await kit.protocol.sessionEventsPage({ sessionId: id });
  const events: HarnessEvent[] = page.events;
  void events;

  // @ts-expect-error The request identifier is a string, not a number.
  await kit.protocol.sessionGet({ sessionId: 42 });
  // @ts-expect-error Every prompt must supply its input parts.
  await kit.protocol.sessionPrompt({ sessionId: id });
  // @ts-expect-error The response wrapper contains a session, not top-level state.
  const state = (await kit.protocol.sessionGet({ sessionId: id })).state;
  void state;

  const patch: SettingsPatch = {
    logging: { level: "info", maxQueueRecords: 64, maxQueueBytes: 65536, maxRecordBytes: 4096 },
    memory: { pinned: false, maxCacheBytes: null },
    budget: { maxTurnTokens: 0 },
    compaction: { autoAtPercent: 80, autoAtTokens: null, timeoutMs: 120_000 },
    session: { options: { enabled: false, count: 0, list: [] } },
  };
  await kit.protocol.settingsConfigure({ patch });
  const stopLogs = await kit.logs.subscribe((notification) => {
    if (notification.record) {
      const message: string = notification.record.message;
      void message;
    }
    if (notification.dropped) {
      const dropped: number = notification.dropped.count;
      void dropped;
    }
  });
  await stopLogs();
  const context = await kit.sessions.context(id);
  const used: number | null = context.usage.usedTokens;
  void used;
  // @ts-expect-error Unknown capacity must be handled before numeric use.
  const capacity: number = context.usage.windowTokens;
  void capacity;
  if (context.canCompact) await kit.sessions.compact(id, { signal: new AbortController().signal });
  // @ts-expect-error Nullable limits accept a number or null, not strings.
  await kit.protocol.settingsConfigure({ patch: { memory: { maxCacheBytes: "unlimited" } } });
  // @ts-expect-error Settings groups are generated, so unknown groups are rejected.
  await kit.protocol.settingsConfigure({ patch: { imaginary: {} } });
}

export function eventNarrowing(event: HarnessEvent): string | undefined {
  if (event.type === "context.usage") {
    const basis: "reported" | "last_request" | "unknown" = event.data.basis;
    return basis;
  }
  if (event.type === "context.compaction") {
    const status: "started" | "completed" | "failed" | "cancelled" = event.data.status;
    return status;
  }
  if (event.type === "message.delta") {
    const text: string = event.data.text;
    return text;
  }
  if (event.type === "turn.completed") {
    // @ts-expect-error A turn completion payload has no message delta text.
    return event.data.text;
  }
  return undefined;
}
