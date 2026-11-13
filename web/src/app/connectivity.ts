// Whether skep-api and the host agent answer, from the outcome of every
// request (ADR 0008, decision 5). The app shell shows a banner while either
// is down; pages keep the data they have, marked with when it was fetched.

import { useSyncExternalStore } from "react";

import { listen, type Outcome } from "@/api/client";

let current: Outcome = "ok";
const subscribers = new Set<() => void>();

function set(next: Outcome) {
  if (next === current) return;
  current = next;
  for (const fn of subscribers) fn();
}

/** Starts listening to the API client. Returns a function that stops. */
export function connectConnectivity(): () => void {
  return listen({ onOutcome: set });
}

function subscribe(fn: () => void) {
  subscribers.add(fn);
  return () => {
    subscribers.delete(fn);
  };
}

/** The latest outcome the API client reported. */
export function useConnectivity(): Outcome {
  return useSyncExternalStore(subscribe, () => current, () => "ok" as Outcome);
}

/** For tests. */
export function resetConnectivity() {
  set("ok");
}
