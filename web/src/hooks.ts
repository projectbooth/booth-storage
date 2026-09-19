import { useCallback, useEffect, useRef, useState } from "react";

export type LoadState<T> =
  | { status: "loading" }
  | { status: "error"; error: string }
  | { status: "ready"; data: T };

export function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/** A hand-rolled fetch state machine (loading → ready | error), as ARCHITECTURE.md §6
 *  recommends over a data-fetching library. Reruns when `deps` change, and ignores a
 *  response that arrives after a newer request was started (a fast workspace/backend
 *  switch must never let a slow, stale response overwrite the current one).
 *
 *  `load` is intentionally not a dependency: it's redefined every render and reading its
 *  latest closure is exactly what's wanted, while `deps` says when to actually re-fetch. */
export function useLoad<T>(load: () => Promise<T>, deps: unknown[]): { state: LoadState<T>; reload: () => void } {
  const [state, setState] = useState<LoadState<T>>({ status: "loading" });
  const latest = useRef(load);
  latest.current = load;
  const generation = useRef(0);

  const run = useCallback((showSpinner: boolean) => {
    const mine = ++generation.current;
    if (showSpinner) setState({ status: "loading" });
    latest.current().then(
      (data) => {
        if (mine === generation.current) setState({ status: "ready", data });
      },
      (err: unknown) => {
        if (mine === generation.current) setState({ status: "error", error: errorMessage(err) });
      },
    );
  }, []);

  useEffect(() => {
    run(true);
  }, deps);

  // A manual reload keeps showing the current data instead of flashing a spinner.
  const reload = useCallback(() => run(false), [run]);
  return { state, reload };
}
