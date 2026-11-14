// Helpers of the VM pages that hold no React state.

import type { VM, VMState } from "@/api/types";
import type { Tone } from "@/components/ui/badge";

export const VM_STATE_TONE: Record<VMState, Tone> = {
  unknown: "neutral",
  stopped: "neutral",
  starting: "info",
  running: "ok",
  stopping: "warn",
  failed: "bad",
};

/** Whether anything of the VM exists on the host: it cannot be changed or deleted. */
export function isActive(vm: VM): boolean {
  const s = vm.observed.state;
  return s === "starting" || s === "running" || s === "stopping";
}

/** Whether the console can be attached (the agent decides). */
export function hasConsole(vm: VM): boolean {
  return vm.observed.state === "running" || vm.observed.state === "stopping";
}

/** The VM name rule of the API (VMCreate.name). */
export const VM_NAME_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$/;

const MIB = 1 << 20;

/**
 * Parses a size as the API accepts it: bytes, or a number with M, G, or T
 * (binary). Returns undefined for anything else.
 */
export function parseSize(text: string): number | undefined {
  const m = /^\s*(\d+)\s*([MmGgTt])?\s*$/.exec(text);
  if (!m) return undefined;
  const n = Number(m[1]);
  const shift = { m: 20, g: 30, t: 40 }[(m[2] ?? "").toLowerCase() as "m" | "g" | "t"] ?? 0;
  const bytes = n * 2 ** shift;
  return Number.isSafeInteger(bytes) ? bytes : undefined;
}

/** Why a size is not acceptable for a disk or for memory, or undefined. */
export function sizeProblem(bytes: number | undefined, minBytes: number, what: string): string | undefined {
  if (bytes === undefined) return `Give the ${what} as a number with M, G, or T, such as 8G.`;
  if (bytes < minBytes) return `The ${what} must be at least ${minBytes / MIB} MiB.`;
  if (bytes % MIB !== 0) return `The ${what} must be a whole number of MiB.`;
  return undefined;
}

/** The message for a VM refusal, when the API's own is not the clearest. */
export function vmErrorHint(code: string): string | undefined {
  switch (code) {
    case "bhyve_unavailable":
      return "bhyve cannot run a VM on this host. The system page says what is missing.";
    case "insufficient_memory":
      return "The running VMs and this one need more memory than the host leaves for guests.";
    case "stop_timeout":
      return "The guest did not shut down in time and is still running. Stop it again, or force-stop it.";
    case "console_busy":
      return "Four clients are already attached to this console.";
  }
  return undefined;
}
