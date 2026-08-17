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
  type: "accepted" | "result" | "error" | "cancelled" | "pong";
  id?: string;
  payload?: unknown;
  error?: string;
  status?: number;
};

interface Pending {
  resolve: (value: unknown) => void;
  reject: (reason: unknown) => void;
}

const BASE = import.meta.env.VITE_API_BASE ?? "/api";
const SUBPROTOCOL = "ode.bearer.token";

/** How long to wait before retrying a dropped connection, and the ceiling. */
const RETRY_MIN_MS = 500;
const RETRY_MAX_MS = 10_000;

function socketUrl(): string {
  // BASE is a path in development, where Vite proxies it, and may be absolute in
  // a deployment. Resolving against the page keeps both working.
  const resolved = new URL(`${BASE}/ws`, window.location.href);
  resolved.protocol = resolved.protocol === "https:" ? "wss:" : "ws:";
  return resolved.toString();
}

export class ProfilerSocket {
  private socket: WebSocket | null = null;
  private connecting: Promise<WebSocket> | null = null;
  private readonly pending = new Map<string, Pending>();
  private sequence = 0;
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
   */
  async request<T>(type: "quick_profiles" | "profile", payload: unknown, signal?: AbortSignal): Promise<T> {
    const id = `r${++this.sequence}`;
    const socket = await this.connect();

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

      const settle = () => signal?.removeEventListener("abort", onAbort);
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

  /** close stops reconnecting and drops the connection, cancelling server work. */
  close() {
    this.closed = true;
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
      const socket = new WebSocket(socketUrl(), protocols);

      return await new Promise<WebSocket>((resolve, reject) => {
        socket.onopen = () => {
          this.socket = socket;
          this.connecting = null;
          this.retryDelay = RETRY_MIN_MS;
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

    const waiting = this.pending.get(frame.id);
    switch (frame.type) {
      case "accepted":
      case "pong":
        // Acknowledgement only; the result is what settles the promise.
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
  }
}

export type SocketState = "idle" | "connecting" | "open" | "reconnecting" | "closed";

/** One socket for the app: the profiler is the only thing that needs it. */
export const profilerSocket = new ProfilerSocket();

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
