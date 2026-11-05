import type {
  HarnessEvent,
  EventPage,
  InputPart,
  ApprovalRequest,
  SubagentUpdatedData,
  ContextUsageData,
  TurnCompletedData,
} from "./protocol.ts";

export interface ConversationMessage {
  id: string;
  seq: number;
  turnId?: string;
  parentCallId?: string;
  role: "user" | "assistant" | "reasoning";
  text: string;
  input?: InputPart[];
  complete: boolean;
}
export interface ConversationSnapshot {
  sessionId: string;
  seq: number;
  messages: ConversationMessage[];
  tools: {
    seq: number;
    callId: string;
    turnId?: string;
    name?: string;
    title?: string;
    completed: boolean;
    data: unknown;
  }[];
  subtasks: SubagentUpdatedData[];
  approvals: ApprovalRequest[];
  turns: { seq: number; turnId: string; result?: TurnCompletedData; status: string }[];
  context?: ContextUsageData;
  /** The configured window or the server's retained history omitted events. */
  truncated: boolean;
  error?: string;
}
export interface ConversationOptions {
  /** Defaults: 2,000 events and 4 MiB of serialized events. */
  maxEvents?: number;
  maxBytes?: number;
}
export interface ConversationSource {
  onEvent(listener: (event: HarnessEvent) => void): () => void;
  sessions: {
    eventsPage(sessionId: string, options: { afterSeq: number; limit: number }): Promise<EventPage>;
  };
}

/** A bounded, framework-independent projection. No filesystem, UI or agent policy. */
export class ConversationState {
  readonly sessionId: string;
  readonly #maxEvents: number;
  readonly #maxBytes: number;
  #events: { event: HarnessEvent; bytes: number }[] = [];
  #bytes = 0;
  #seq = 0;
  #truncated = false;
  constructor(sessionId: string, options: ConversationOptions = {}) {
    this.sessionId = sessionId;
    this.#maxEvents = options.maxEvents ?? 2000;
    this.#maxBytes = options.maxBytes ?? 4 * 1024 * 1024;
    for (const limit of [this.#maxEvents, this.#maxBytes])
      if (!Number.isSafeInteger(limit) || limit < 1)
        throw new RangeError("Conversation limits must be positive safe integers.");
  }
  get seq(): number {
    return this.#seq;
  }
  markTruncated(): void {
    this.#truncated = true;
  }
  apply(event: HarnessEvent): boolean {
    if (event.sessionId !== this.sessionId || event.seq <= this.#seq || event.type === "stream.gap")
      return false;
    this.#seq = event.seq;
    const bytes = new TextEncoder().encode(JSON.stringify(event)).byteLength;
    if (bytes > this.#maxBytes) {
      this.#truncated = true;
      return true;
    }
    this.#events.push({ event: structuredClone(event), bytes });
    this.#bytes += bytes;
    while (this.#events.length > this.#maxEvents || this.#bytes > this.#maxBytes) {
      this.#bytes -= this.#events.shift()!.bytes;
      this.#truncated = true;
    }
    return true;
  }
  getSnapshot(): ConversationSnapshot {
    const messages = new Map<string, ConversationMessage>();
    const tools = new Map<string, ConversationSnapshot["tools"][number]>();
    const subtasks = new Map<string, SubagentUpdatedData>();
    const approvals = new Map<string, ApprovalRequest>();
    const turns = new Map<string, ConversationSnapshot["turns"][number]>();
    let context: ContextUsageData | undefined;
    for (const { event: e } of this.#events) {
      const turnId = e.turnId;
      switch (e.type) {
        case "turn.started": {
          const id = `${turnId}:user`;
          messages.set(id, {
            id,
            seq: e.seq,
            turnId,
            role: "user",
            text: e.data.input?.[0]?.text ?? e.data.text ?? "",
            input: e.data.input,
            complete: true,
          });
          if (turnId) turns.set(turnId, { seq: e.seq, turnId, status: "running" });
          break;
        }
        case "message.delta":
        case "message.completed":
        case "reasoning.delta":
        case "reasoning.completed": {
          const role = e.type.startsWith("reasoning") ? "reasoning" : "assistant";
          const d = e.data;
          const parentCallId = "parentCallId" in d ? d.parentCallId : undefined;
          const id = `${turnId}:${parentCallId ?? ""}:${role}:${d.itemId}`;
          const message = messages.get(id) ?? {
            id,
            seq: e.seq,
            turnId,
            parentCallId,
            role,
            text: "",
            complete: false,
          };
          message.complete = e.type.endsWith("completed");
          message.text = message.complete ? d.text : message.text + d.text;
          messages.set(id, message);
          break;
        }
        case "tool.started":
          tools.set(e.data.callId, {
            seq: e.seq,
            callId: e.data.callId,
            turnId,
            name: e.data.name,
            title: e.data.title,
            completed: false,
            data: e.data,
          });
          break;
        case "tool.completed":
          tools.set(e.data.callId, {
            seq: e.seq,
            ...tools.get(e.data.callId),
            callId: e.data.callId,
            turnId,
            completed: true,
            data: e.data,
          });
          break;
        case "subagent.updated": {
          const d = e.data;
          const previous = [...subtasks.values()].find(
            (task) => task.id === d.id || (!!d.callId && task.callId === d.callId),
          );
          if (previous && previous.id !== d.id) subtasks.delete(previous.id);
          subtasks.set(d.id, { ...previous, ...d });
          break;
        }
        case "approval.requested":
          approvals.set(e.data.approvalId, e.data);
          break;
        case "approval.resolved":
          approvals.delete(e.data.approvalId);
          break;
        case "turn.completed":
          if (turnId)
            turns.set(turnId, { seq: e.seq, turnId, status: e.data.status, result: e.data });
          for (const [id, approval] of approvals)
            if (approval.turnId === turnId) approvals.delete(id);
          break;
        case "context.usage":
          context = e.data;
          break;
      }
    }
    return structuredClone({
      sessionId: this.sessionId,
      seq: this.#seq,
      messages: [...messages.values()],
      tools: [...tools.values()],
      subtasks: [...subtasks.values()],
      approvals: [...approvals.values()],
      turns: [...turns.values()],
      context,
      truncated: this.#truncated,
    });
  }
}

/** Subscribe before replay, recover gaps, and stop with close(). Reading never
 * restores a runtime or sends a model request. Each observer owns its limits. */
export class Conversation {
  readonly ready: Promise<void>;
  readonly #source: ConversationSource;
  readonly #state: ConversationState;
  readonly #listeners = new Set<(snapshot: ConversationSnapshot) => void>();
  readonly #unsubscribe: () => void;
  #loading = false;
  #wanted = 0;
  #resync = false;
  #closed = false;
  #error: string | undefined;
  #refresh: Promise<void> | undefined;
  constructor(source: ConversationSource, sessionId: string, options: ConversationOptions = {}) {
    this.#source = source;
    this.#state = new ConversationState(sessionId, options);
    // Set before subscribing so a synchronous source cannot race initial replay.
    this.#loading = true;
    this.#unsubscribe = source.onEvent((event) => {
      if (this.#closed || event.sessionId !== sessionId) return;
      if (event.type === "stream.gap") {
        this.#resync = true;
        if (!this.#loading) void this.refresh().catch(() => {});
      } else if (this.#loading) this.#wanted = Math.max(this.#wanted, event.seq);
      else if (this.#state.apply(event)) this.#notify();
    });
    this.ready = this.refresh();
    this.ready.catch(() => {});
  }
  getSnapshot(): ConversationSnapshot {
    return { ...this.#state.getSnapshot(), ...(this.#error ? { error: this.#error } : {}) };
  }
  subscribe(listener: (snapshot: ConversationSnapshot) => void): () => void {
    if (this.#closed) throw new Error("Conversation observer is closed.");
    this.#listeners.add(listener);
    return () => {
      this.#listeners.delete(listener);
    };
  }
  #notify(): void {
    const listeners = [...this.#listeners]; // A callback may subscribe/unsubscribe.
    if (!this.#closed)
      for (const listener of listeners) {
        try {
          listener(this.getSnapshot());
        } catch (error) {
          queueMicrotask(() => {
            throw error;
          });
        }
      }
  }
  refresh(): Promise<void> {
    if (this.#closed) return Promise.reject(new Error("Conversation observer is closed."));
    if (this.#refresh) return this.#refresh;
    this.#loading = true;
    this.#refresh = this.#replay()
      .catch((error) => {
        this.#error = String(error instanceof Error ? error.message : error);
        this.#notify();
        throw error;
      })
      .finally(() => {
        this.#loading = false;
        this.#refresh = undefined;
        if (!this.#closed && !this.#error && this.#resync) void this.refresh().catch(() => {});
      });
    return this.#refresh;
  }
  async #replay(): Promise<void> {
    let cursor = this.#state.seq;
    do {
      this.#resync = false;
      const page = await this.#source.sessions.eventsPage(this.#state.sessionId, {
        afterSeq: cursor,
        limit: 200,
      });
      if (this.#closed) return;
      if (page.truncated) this.#state.markTruncated();
      for (const event of page.events) this.#state.apply(event);
      this.#error = undefined;
      this.#notify();
      if (!page.hasMore && !this.#resync && this.#wanted <= this.#state.seq) {
        this.#loading = false;
        return;
      }
      if (page.nextSeq <= cursor)
        throw new Error(
          "Conversation history did not advance; retained history may be incomplete.",
        );
      cursor = page.nextSeq;
    } while (!this.#closed);
  }
  close(): void {
    if (!this.#closed) {
      this.#closed = true;
      this.#unsubscribe();
      this.#listeners.clear();
    }
  }
}
