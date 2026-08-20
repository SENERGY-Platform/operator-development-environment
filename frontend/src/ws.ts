/*
 * Copyright 2026 InfAI (CC SES)
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

import type { ChatEvent, KernelEvent } from "./api";
import { token } from "./keycloak";

/**
 * The profiler runs over a WebSocket because a read outlives an HTTP request.
 *
 * A raw pass bounded at a hundred thousand points is megabytes of JSON per column
 * from the platform, which an ingress idle timeout will cut off — and an aborted
 * HTTP request leaves the backend reading for a client that has gone. Here every
 * request has an id, the backend can be told to stop, and closing the socket stops
 * everything it was doing.
 */

export class WsError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
    this.name = "WsError";
  }

  /** 403 is final: the account lacks the developer role. */
  get isForbidden(): boolean {
    return this.status === 403;
  }
}

/** Cancelled is thrown into the caller's promise when a request is aborted. */
export class Cancelled extends Error {
  constructor() {
    super("cancelled");
    this.name = "Cancelled";
  }
}

type Inbound = {
  type: "accepted" | "result" | "event" | "done" | "error" | "cancelled" | "pong";
  id?: string;
  payload?: unknown;
  error?: string;
  status?: number;
};

interface Pending {
  resolve: (value: unknown) => void;
  reject: (reason: unknown) => void;
}

/**
 * A subscription to a chat exchange.
 *
 * Separate from Pending because the two message shapes differ in kind: a profiler
 * operation answers once with a result, whereas an exchange emits many events and
 * then finishes. Routing both through one promise would mean either buffering the
 * whole turn before showing any of it, or resolving on the first event.
 */
interface Stream {
  onEvent: (event: unknown) => void;
  resolve: (value: StreamOutcome) => void;
  reject: (reason: unknown) => void;
}

/** StreamOutcome says whether there was a turn to watch — see chat_attach. */
export interface StreamOutcome {
  attached: boolean;
}

const BASE = import.meta.env.VITE_API_BASE ?? "/api";
const SUBPROTOCOL = "ode.bearer.token";

/** How long to wait before retrying a dropped connection, and the ceiling. */
const RETRY_MIN_MS = 500;
const RETRY_MAX_MS = 10_000;

/**
 * How often to check, while connected, whether the access token has been renewed.
 *
 * On a timer rather than only before each request, which is the opposite of what
 * `keycloak.ts` does for HTTP — and for a reason. A chat turn runs detached on the
 * backend and emits events for minutes without the client sending anything, so
 * there is no request to hang the refresh off. The check is local unless the token
 * is nearly expired, and the auth frame only goes out when the string changed.
 */
const TOKEN_POLL_MS = 20_000;

function socketUrl(): string {
  // BASE is a path in development, where Vite proxies it, and may be absolute in
  // a deployment. Resolving against the page keeps both working.
  const resolved = new URL(`${BASE}/ws`, window.location.href);
  resolved.protocol = resolved.protocol === "https:" ? "wss:" : "ws:";
  return resolved.toString();
}

/**
 * OdeSocket is ODE's single streaming connection.
 *
 * It carries both surfaces: the profiler's one-shot operations, and the chat
 * exchange. Having one is the point — a profile read outlives an HTTP request, and
 * so does a chat turn that runs one, so both need the same liveness (the ping in
 * ws.go) and the same cancellation.
 */
export class OdeSocket {
  private socket: WebSocket | null = null;
  private connecting: Promise<WebSocket> | null = null;
  private readonly pending = new Map<string, Pending>();
  private readonly streams = new Map<string, Stream>();
  /** Progress handlers for in-flight requests, keyed by operation id. */
  private readonly phases = new Map<string, (phase: unknown) => void>();
  private sequence = 0;
  /** The access token the backend currently holds for this connection. */
  private socketToken: string | undefined;
  private refreshing: Promise<void> | null = null;
  private tokenTimer: number | undefined;
  private retryDelay = RETRY_MIN_MS;
  private closed = false;
  private listeners = new Set<(state: SocketState) => void>();
  private state: SocketState = "idle";

  /** onState reports connection state so the UI can say when it is offline. */
  onState(listener: (state: SocketState) => void): () => void {
    this.listeners.add(listener);
    listener(this.state);
    return () => this.listeners.delete(listener);
  }

  private setState(state: SocketState) {
    if (this.state === state) return;
    this.state = state;
    for (const listener of this.listeners) listener(state);
  }

  /**
   * request sends one operation and resolves with its result.
   *
   * The AbortSignal is the whole point: aborting sends a cancel for this id, so
   * the backend stops its platform reads rather than finishing work nobody will
   * look at.
   *
   * onPhase receives the operation's progress frames. Optional, and dropping one
   * costs a progress line rather than part of the answer — unlike a chat stream,
   * where the events *are* the answer. M6's relational pass is what needs it: it
   * profiles every participating service before it aligns them, so it is the longest
   * thing a developer waits on.
   */
  async request<T>(
    type: "quick_profiles" | "profile" | "resolve_selection" | "relate",
    payload: unknown,
    signal?: AbortSignal,
    onPhase?: (phase: OperationPhase) => void,
  ): Promise<T> {
    const id = `r${++this.sequence}`;
    const socket = await this.ready();
    if (onPhase) this.phases.set(id, onPhase as (phase: unknown) => void);

    return new Promise<T>((resolve, reject) => {
      if (signal?.aborted) {
        reject(new Cancelled());
        return;
      }

      this.pending.set(id, {
        resolve: resolve as (value: unknown) => void,
        reject,
      });

      const onAbort = () => {
        // Tell the backend, then settle locally. The cancelled frame that comes
        // back finds nothing pending, which is fine and expected.
        this.trySend({ type: "cancel", id });
        const waiting = this.pending.get(id);
        this.pending.delete(id);
        waiting?.reject(new Cancelled());
      };
      signal?.addEventListener("abort", onAbort, { once: true });

      const settle = () => {
        this.phases.delete(id);
        signal?.removeEventListener("abort", onAbort);
      };
      const wrapped = this.pending.get(id);
      if (wrapped) {
        this.pending.set(id, {
          resolve: (value) => {
            settle();
            wrapped.resolve(value);
          },
          reject: (reason) => {
            settle();
            wrapped.reject(reason);
          },
        });
      }

      try {
        socket.send(JSON.stringify({ type, id, payload }));
      } catch (e: unknown) {
        this.pending.delete(id);
        settle();
        reject(e);
      }
    });
  }

  /**
   * stream subscribes to a chat exchange and resolves when the turn ends.
   *
   * Aborting detaches this view and leaves the exchange running, which is what
   * closing a tab should do. Stopping the work is a separate act — see cancelChat —
   * because the backend's exchange is detached from any connection.
   */
  async stream(
    type: "chat_send" | "chat_confirm" | "chat_attach",
    payload: unknown,
    handlers: { onEvent: (event: ChatEvent) => void; signal?: AbortSignal },
  ): Promise<StreamOutcome> {
    const id = `s${++this.sequence}`;
    const socket = await this.ready();

    return new Promise<StreamOutcome>((resolve, reject) => {
      if (handlers.signal?.aborted) {
        reject(new Cancelled());
        return;
      }

      const finish = () => {
        this.streams.delete(id);
        handlers.signal?.removeEventListener("abort", onAbort);
      };
      const onAbort = () => {
        // Detach the view. The exchange keeps running server-side.
        this.trySend({ type: "cancel", id });
        finish();
        reject(new Cancelled());
      };
      handlers.signal?.addEventListener("abort", onAbort, { once: true });

      this.streams.set(id, {
        onEvent: handlers.onEvent as (event: unknown) => void,
        resolve: (value) => {
          finish();
          resolve(value);
        },
        reject: (reason) => {
          finish();
          reject(reason);
        },
      });

      try {
        socket.send(JSON.stringify({ type, id, payload }));
      } catch (e: unknown) {
        finish();
        reject(e);
      }
    });
  }

  /**
   * execute runs one cell in the developer's own pod and streams what it produces.
   *
   * The cancellation rule here is the opposite of the chat one above, deliberately.
   * Aborting a chat stream detaches a view and leaves the turn running, because an
   * answer nobody is watching is still worth having. Aborting an execution
   * *interrupts the cell*, because a training loop nobody is watching is only
   * costing the developer their own pod.
   *
   * The promise is not settled by the abort. The backend keeps relaying after the
   * interrupt, so the final event — status `interrupted` — still arrives, and the
   * developer sees how their cell ended rather than the UI simply going quiet.
   */
  async execute(
    code: string,
    handlers: { onEvent: (event: KernelEvent) => void; signal?: AbortSignal },
  ): Promise<StreamOutcome> {
    const id = `k${++this.sequence}`;
    const socket = await this.ready();

    return new Promise<StreamOutcome>((resolve, reject) => {
      if (handlers.signal?.aborted) {
        reject(new Cancelled());
        return;
      }

      const finish = () => {
        this.streams.delete(id);
        handlers.signal?.removeEventListener("abort", onAbort);
      };
      // Interrupt only. The `done` frame that follows is what settles this.
      const onAbort = () => this.trySend({ type: "cancel", id });
      handlers.signal?.addEventListener("abort", onAbort, { once: true });

      this.streams.set(id, {
        onEvent: handlers.onEvent as (event: unknown) => void,
        resolve: (value) => {
          finish();
          resolve(value);
        },
        reject: (reason) => {
          finish();
          reject(reason);
        },
      });

      try {
        socket.send(JSON.stringify({ type: "kernel_execute", id, payload: { code } }));
      } catch (e: unknown) {
        finish();
        reject(e);
      }
    });
  }

  /**
   * cancelChat abandons the turn running on a session.
   *
   * Distinct from aborting a stream, which only detaches this client's view.
   */
  cancelChat(sessionId: string) {
    this.trySend({
      type: "chat_cancel",
      id: `c${++this.sequence}`,
      payload: { session_id: sessionId },
    });
  }

  /** close stops reconnecting and drops the connection, cancelling server work. */
  close() {
    this.closed = true;
    this.stopTokenPolling();
    this.rejectAll(new Cancelled());
    this.socket?.close();
    this.socket = null;
    this.setState("closed");
  }

  private trySend(message: unknown) {
    if (this.socket?.readyState === WebSocket.OPEN) {
      this.socket.send(JSON.stringify(message));
    }
  }

  /**
   * ready is connect plus a current token.
   *
   * The handshake authenticates once and this connection then lives as long as the
   * tab, but the access token behind it expires — so the backend has to be handed
   * the refreshed one, or its next platform read goes out with an expired
   * credential and comes back 401.
   */
  private async ready(): Promise<WebSocket> {
    const socket = await this.connect();
    await this.refreshServerToken(socket);
    return socket;
  }

  /**
   * refreshServerToken hands the backend the current access token if it has
   * changed.
   *
   * `token()` only talks to Keycloak when the token is within thirty seconds of
   * expiring, so this costs nothing on most calls, and the auth frame is only sent
   * when the string actually changed — roughly once per token lifetime. Waiting for
   * the acknowledgement is deliberate: a refusal (a revoked role, say) should
   * surface here, as itself, rather than as an upstream failure two calls later.
   */
  private async refreshServerToken(socket: WebSocket): Promise<void> {
    if (this.refreshing) return this.refreshing;

    this.refreshing = (async () => {
      const accessToken = await token();
      if (!accessToken || accessToken === this.socketToken) return;

      const id = `a${++this.sequence}`;
      await new Promise<void>((resolve, reject) => {
        this.pending.set(id, { resolve: () => resolve(), reject });
        try {
          socket.send(JSON.stringify({ type: "auth", id, payload: { token: accessToken } }));
        } catch (e: unknown) {
          this.pending.delete(id);
          reject(e);
        }
      });
      this.socketToken = accessToken;
    })();

    try {
      await this.refreshing;
    } finally {
      this.refreshing = null;
    }
  }

  private startTokenPolling(socket: WebSocket) {
    this.stopTokenPolling();
    this.tokenTimer = window.setInterval(() => {
      if (this.socket !== socket) return;
      void this.refreshServerToken(socket).catch((e: unknown) => {
        // The backend refused the credential — a revoked role, or a token it will
        // not accept. Dropping the connection is the honest response: the
        // reconnect re-runs the handshake, which either works or fails with the
        // real reason where the UI already reports it.
        if (e instanceof WsError) socket.close();
      });
    }, TOKEN_POLL_MS);
  }

  private stopTokenPolling() {
    if (this.tokenTimer !== undefined) {
      window.clearInterval(this.tokenTimer);
      this.tokenTimer = undefined;
    }
  }

  private async connect(): Promise<WebSocket> {
    if (this.socket?.readyState === WebSocket.OPEN) return this.socket;
    if (this.connecting) return this.connecting;

    this.closed = false;
    this.setState("connecting");

    this.connecting = (async () => {
      const accessToken = await token();
      // A browser cannot set an Authorization header on a WebSocket handshake, so
      // the token travels as a subprotocol. The backend also accepts a query
      // parameter, which is avoided here because it would end up in access logs.
      const protocols = accessToken ? [`${SUBPROTOCOL}.${accessToken}`] : undefined;
      // What the backend holds from here on, until an auth frame replaces it.
      this.socketToken = accessToken;
      const socket = new WebSocket(socketUrl(), protocols);

      return await new Promise<WebSocket>((resolve, reject) => {
        socket.onopen = () => {
          this.socket = socket;
          this.connecting = null;
          this.retryDelay = RETRY_MIN_MS;
          this.startTokenPolling(socket);
          this.setState("open");
          resolve(socket);
        };
        socket.onerror = () => {
          // The close handler does the reporting; onerror carries no detail in a
          // browser by design.
        };
        socket.onclose = (event) => {
          this.socket = null;
          this.connecting = null;
          this.stopTokenPolling();
          const reason =
            event.code === 1006
              ? "the connection dropped — check that the backend is reachable and that the account has the developer role"
              : event.reason || `connection closed (${event.code})`;
          reject(new WsError(0, reason));
          this.onClosed(reason);
        };
        socket.onmessage = (event) => this.onMessage(event);
      });
    })();

    try {
      return await this.connecting;
    } catch (e: unknown) {
      this.connecting = null;
      throw e;
    }
  }

  private onMessage(event: MessageEvent) {
    let frame: Inbound;
    try {
      frame = JSON.parse(String(event.data)) as Inbound;
    } catch {
      return;
    }
    if (!frame.id) return;

    // Chat streams first: they own their ids and settle on done rather than result.
    const stream = this.streams.get(frame.id);
    if (stream) {
      switch (frame.type) {
        case "event":
          stream.onEvent(frame.payload);
          return;
        case "done":
          stream.resolve((frame.payload as StreamOutcome) ?? { attached: true });
          return;
        case "cancelled":
          stream.reject(new Cancelled());
          return;
        case "error":
          stream.reject(new WsError(frame.status ?? 0, frame.error ?? "unknown error"));
          return;
        default:
          // accepted, pong: acknowledgement only.
          return;
      }
    }

    const waiting = this.pending.get(frame.id);
    switch (frame.type) {
      case "accepted":
      case "pong":
        // Acknowledgement only; the result is what settles the promise.
        return;
      case "event":
        // A progress frame for a request. Ignored when nobody asked for phases,
        // which is every operation but the relational pass.
        this.phases.get(frame.id)?.(frame.payload);
        return;
      case "result":
        this.pending.delete(frame.id);
        waiting?.resolve(frame.payload);
        return;
      case "cancelled":
        this.pending.delete(frame.id);
        waiting?.reject(new Cancelled());
        return;
      case "error":
        this.pending.delete(frame.id);
        waiting?.reject(new WsError(frame.status ?? 0, frame.error ?? "unknown error"));
        return;
    }
  }

  private onClosed(reason: string) {
    // Everything in flight died with the connection, and the backend has already
    // cancelled it. Failing the promises is more honest than waiting for results
    // that will never come.
    this.rejectAll(new WsError(0, reason));

    if (this.closed) {
      this.setState("closed");
      return;
    }
    this.setState("reconnecting");
    const delay = this.retryDelay;
    this.retryDelay = Math.min(this.retryDelay * 2, RETRY_MAX_MS);
    window.setTimeout(() => {
      if (this.closed) return;
      // Reconnect eagerly so the UI shows "connected" again without a click. A
      // failure here schedules the next attempt through the same path.
      void this.connect().catch(() => undefined);
    }, delay);
  }

  private rejectAll(reason: unknown) {
    const waiting = [...this.pending.values()];
    this.pending.clear();
    for (const entry of waiting) entry.reject(reason);

    // Chat streams are failed too, but the exchange behind one is *not* over: it
    // is detached and still running. The caller reattaches on reconnect rather
    // than treating this as a lost turn — see ChatView.
    const streams = [...this.streams.values()];
    this.streams.clear();
    for (const entry of streams) entry.reject(reason);
  }
}

/** One step of a long-running operation, as the backend reports it. */
export interface OperationPhase {
  stage: string;
  detail: string;
}

export type SocketState = "idle" | "connecting" | "open" | "reconnecting" | "closed";

/** One socket for the app, carrying both the profiler operations and chat. */
export const odeSocket = new OdeSocket();

/**
 * profilerSocket is the former name, kept so the profiler and selection views read
 * unchanged. Same object.
 */
export const profilerSocket = odeSocket;

export function describeWsError(e: unknown): string {
  if (e instanceof Cancelled) return "cancelled";
  if (e instanceof WsError) {
    if (e.isForbidden) {
      return "Forbidden. This account is missing the `developer` realm role, or may not read this resource.";
    }
    return e.status ? `${e.status}: ${e.message}` : e.message;
  }
  return e instanceof Error ? e.message : String(e);
}
