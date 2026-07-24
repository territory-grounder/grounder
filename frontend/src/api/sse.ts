// The console subscribes to a Server-Sent Events stream for live decision/ledger updates. A dropped stream
// must surface a disconnected indicator and auto-reconnect with backoff — the console degrades visibly, it
// never silently shows stale data as live.
export type SseStatus = "connecting" | "connected" | "disconnected";

export interface SseHandle {
  status: () => SseStatus;
  close: () => void;
}

// subscribe wires an EventSource-like source; onStatus fires on connect/disconnect so the UI can render the
// indicator. The factory is injectable so tests drive disconnect without a real network.
export function subscribe(
  make: () => { onopen: (() => void) | null; onerror: (() => void) | null; close: () => void },
  onStatus: (s: SseStatus) => void,
): SseHandle {
  let status: SseStatus = "connecting";
  onStatus(status);
  const src = make();
  src.onopen = () => {
    status = "connected";
    onStatus(status);
  };
  src.onerror = () => {
    status = "disconnected";
    onStatus(status); // the UI shows the disconnected indicator; reconnect is scheduled by the caller
  };
  return { status: () => status, close: () => src.close() };
}
