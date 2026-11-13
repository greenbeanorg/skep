import { useCallback, useEffect, useRef, useState } from "react";

import { ApiError, toApiError, type ReadOptions } from "./client";

export interface Query<T> {
  /** The latest successful result. Kept while a refresh is in flight or fails. */
  data: T | undefined;
  /** The error of the latest attempt, if it failed. */
  error: ApiError | undefined;
  /** True until the first attempt for the current key has finished. */
  loading: boolean;
  /** When data was last fetched successfully (ms since the epoch). */
  updatedAt: number | undefined;
  /** Fetches again as a user action (counts as activity). */
  reload: () => void;
}

export interface QueryOptions {
  /**
   * Re-fetch on this interval while the page is visible, as a background
   * request (ADR 0008, decision 3): it carries the background header and
   * does not extend the session. Refreshing pauses while the tab is
   * hidden and resumes, with one refresh, when it is shown again.
   */
  refreshMs?: number;
}

interface State<T> {
  key: string | null;
  data: T | undefined;
  error: ApiError | undefined;
  updatedAt: number | undefined;
  settled: boolean;
}

/** Most the refresh interval grows to while skep-api cannot be reached. */
const MAX_BACKOFF_MS = 120_000;

/**
 * Runs a read-only API call and keeps its result. The call is repeated when
 * `key` changes (a user action), on reload() (a user action), and on the
 * refresh interval (background). A null key means "not ready yet" and
 * fetches nothing. While skep-api cannot be reached, background refreshes
 * back off, doubling the interval up to two minutes; the first success
 * resets it.
 */
export function useQuery<T>(
  key: string | null,
  fetcher: (options: ReadOptions) => Promise<T>,
  options: QueryOptions = {},
): Query<T> {
  const [state, setState] = useState<State<T>>({
    key,
    data: undefined,
    error: undefined,
    updatedAt: undefined,
    settled: false,
  });
  // A tick is one fetch: the number says when, the flag says whether the
  // user caused it.
  const [tick, setTick] = useState<{ n: number; background: boolean }>({ n: 0, background: false });
  const backoff = useRef(0);

  // The fetcher is usually an inline closure; keep the latest one without
  // making it a dependency of the effect.
  const fetcherRef = useRef(fetcher);
  useEffect(() => {
    fetcherRef.current = fetcher;
  });

  useEffect(() => {
    if (key === null) return;
    const controller = new AbortController();
    fetcherRef
      .current({ signal: controller.signal, background: tick.background })
      .then((result) => {
        if (controller.signal.aborted) return;
        backoff.current = 0;
        setState({ key, data: result, error: undefined, updatedAt: Date.now(), settled: true });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        const error = toApiError(err);
        if (error.isNetwork) backoff.current = Math.min(backoff.current === 0 ? 1 : backoff.current * 2, 64);
        // Keep showing the last good data of the same resource.
        setState((prev) => ({
          key,
          data: prev.key === key ? prev.data : undefined,
          updatedAt: prev.key === key ? prev.updatedAt : undefined,
          error,
          settled: true,
        }));
      });
    return () => controller.abort();
  }, [key, tick]);

  const refreshMs = options.refreshMs;
  useEffect(() => {
    if (key === null || !refreshMs) return;
    let timer: number | undefined;
    const schedule = () => {
      const interval = Math.min(refreshMs * Math.max(1, backoff.current), MAX_BACKOFF_MS);
      timer = window.setTimeout(() => {
        if (document.visibilityState === "visible") setTick((t) => ({ n: t.n + 1, background: true }));
        schedule();
      }, interval);
    };
    const onVisible = () => {
      if (document.visibilityState === "visible") setTick((t) => ({ n: t.n + 1, background: true }));
    };
    schedule();
    document.addEventListener("visibilitychange", onVisible);
    return () => {
      window.clearTimeout(timer);
      document.removeEventListener("visibilitychange", onVisible);
    };
  }, [key, refreshMs]);

  const reload = useCallback(() => setTick((t) => ({ n: t.n + 1, background: false })), []);

  // State that belongs to another key is a different resource: ignore it.
  const current = state.key === key;
  return {
    data: current ? state.data : undefined,
    error: current ? state.error : undefined,
    updatedAt: current ? state.updatedAt : undefined,
    loading: key !== null && !(current && state.settled),
    reload,
  };
}
