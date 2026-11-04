// Compile-only API assertions. These functions are never executed.
import type { HarnessEvent, HarnessKit, SettingsPatch } from "../../src/index.ts";

export async function generatedMethods(kit: HarnessKit): Promise<void> {
  const { session } = await kit.protocol.sessionCreate({ settings: { session: { runtime: "fake" } } });
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
    memory: { pinned: false, maxCacheBytes: null },
    budget: { maxTurnTokens: 0 },
    session: { options: { enabled: false, count: 0, list: [] } },
  };
  await kit.protocol.settingsConfigure({ patch });
  // @ts-expect-error Nullable limits accept a number or null, not strings.
  await kit.protocol.settingsConfigure({ patch: { memory: { maxCacheBytes: "unlimited" } } });
  // @ts-expect-error Settings groups are generated, so unknown groups are rejected.
  await kit.protocol.settingsConfigure({ patch: { imaginary: {} } });
}

export function eventNarrowing(event: HarnessEvent): string | undefined {
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
