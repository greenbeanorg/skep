import { useCallback, useEffect, useRef, useState } from "react";

import { ApiError } from "./client";

export interface Query<T> {
  /** The latest successful result. Kept while a refresh is in flight or fails. */
  data: T | undefined;
  /** The error of the latest attempt, if it failed. */
  error: ApiError | undefined;
  /** True until the first attempt for the current key has finished. */
  loading: boolean;
  /** When data was last fetched successfully (ms since the epoch). */
  updatedAt: number | undefined;
  reload: () => void;
}

export interface QueryOptions {
  /** Re-fetch on this interval while the page is visible. */
  refreshMs?: number;
}

interface State<T> {
  key: string | null;
  data: T | undefined;
  error: ApiError | undefined;
  updatedAt: number | undefined;
  settled: boolean;
}

/**
 * Runs a read-only API call and keeps its result. The call is repeated when
 * `key` changes, on reload(), and on the refresh interval. A null key means
 * "not ready yet" and fetches nothing.
 */
export function useQuery<T>(
  key: string | null,
  fetcher: (signal: AbortSignal) => Promise<T>,
  options: QueryOptions = {},
): Query<T> {
  const [state, setState] = useState<State<T>>({
    key,
    data: undefined,
    error: undefined,
    updatedAt: undefined,
    settled: false,
  });
  const [tick, setTick] = useState(0);

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
      .current(controller.signal)
      .then((result) => {
        if (controller.signal.aborted) return;
        setState({ key, data: result, error: undefined, updatedAt: Date.now(), settled: true });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        const error =
          err instanceof ApiError ? err : new ApiError(0, "client_error", "The response could not be processed.");
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
    const id = window.setInterval(() => {
      if (document.visibilityState === "visible") setTick((t) => t + 1);
    }, refreshMs);
    return () => window.clearInterval(id);
  }, [key, refreshMs]);

  const reload = useCallback(() => setTick((t) => t + 1), []);

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
