// Helpers of the container (jail) pages that hold no React state. Every
// rule here is checked again by the API; the UI uses them only to explain
// a refusal before the request is sent.

import type {
  Container,
  ContainerCapabilities,
  ContainerLimits,
  ContainerMetrics,
  ContainerNICSpec,
  ContainerState,
  Metrics,
} from "@/api/types";
import type { Tone } from "@/components/ui/badge";

import { parseSize } from "../vms/vm";

export const CONTAINER_STATE_TONE: Record<ContainerState, Tone> = {
  unknown: "neutral",
  stopped: "neutral",
  starting: "info",
  running: "ok",
  stopping: "warn",
  failed: "bad",
};

/** The jail name rule of the API (ContainerCreate.name). */
export const CONTAINER_NAME_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$/;
export const NAME_HELP = "1 to 63 characters: a letter or digit, then letters, digits, '.', '_' or '-'.";

/** At most this many NICs (models.MaxContainerNICs) and nameservers. */
export const MAX_NICS = 8;
export const MAX_NAMESERVERS = 3;

/** Whether a jail exists on the host: it cannot be deleted or rolled back. */
export function isActive(c: Container): boolean {
  return c.state === "starting" || c.state === "running" || c.state === "stopping";
}

const MIB = 2 ** 20;

/** Minimums the API enforces (ContainerLimits). */
export const LIMIT_MIN = {
  memory: 16 * MIB,
  openFiles: 64,
  diskQuota: 64 * MIB,
};

/** What a user types for the limits; empty means no limit. */
export interface LimitsForm {
  memory: string;
  locked_memory: string;
  cpu_percent: string;
  max_processes: string;
  open_files: string;
  disk_quota: string;
}

export const EMPTY_LIMITS: LimitsForm = {
  memory: "",
  locked_memory: "",
  cpu_percent: "",
  max_processes: "",
  open_files: "",
  disk_quota: "",
};

/** The limits that are RCTL rules and need RACCT; disk_quota is ZFS. */
export const RCTL_LIMITS: (keyof LimitsForm)[] = ["memory", "locked_memory", "cpu_percent", "max_processes", "open_files"];

function parseCount(text: string): number | undefined {
  const t = text.trim();
  if (!/^\d+$/.test(t)) return undefined;
  const n = Number(t);
  return Number.isSafeInteger(n) ? n : undefined;
}

export interface ParsedLimits {
  limits: ContainerLimits;
  problems: Partial<Record<keyof LimitsForm, string>>;
}

/**
 * Turns the limit fields into a ContainerLimits, with a problem per field
 * the API would refuse. `caps` is GET /system's containers block; without
 * it (an older API) the RACCT and CPU bounds are left to the server.
 */
export function parseLimits(form: LimitsForm, caps: ContainerCapabilities | undefined): ParsedLimits {
  const limits: ContainerLimits = {};
  const problems: ParsedLimits["problems"] = {};
  const size = (key: "memory" | "locked_memory" | "disk_quota", min: number, what: string) => {
    if (form[key].trim() === "") return;
    const bytes = parseSize(form[key]);
    if (bytes === undefined) problems[key] = `Give the ${what} as a number with M, G, or T, such as 512M.`;
    else if (bytes < min) problems[key] = `The ${what} must be at least ${min / MIB} MiB.`;
    else limits[key] = bytes;
  };
  const count = (key: "cpu_percent" | "max_processes" | "open_files", min: number, max: number | undefined, what: string) => {
    if (form[key].trim() === "") return;
    const n = parseCount(form[key]);
    if (n === undefined || n < min) problems[key] = `Give ${what} as a whole number of at least ${min}.`;
    else if (max !== undefined && n > max) problems[key] = `At most ${max} on this host (100 per CPU).`;
    else limits[key] = n;
  };
  size("memory", LIMIT_MIN.memory, "memory limit");
  size("locked_memory", 1, "locked-memory limit");
  size("disk_quota", LIMIT_MIN.diskQuota, "disk quota");
  count("cpu_percent", 1, caps?.max_cpu_percent || undefined, "the CPU limit");
  count("max_processes", 1, undefined, "the process limit");
  count("open_files", LIMIT_MIN.openFiles, undefined, "the file-table limit");
  if (limits.memory !== undefined && limits.locked_memory !== undefined && (limits.locked_memory as number) > (limits.memory as number)) {
    problems.locked_memory = "The locked-memory limit cannot be more than the memory limit.";
  }
  if (caps && !caps.rctl) {
    for (const key of RCTL_LIMITS) {
      if (form[key].trim() !== "" && !problems[key]) problems[key] = RACCT_OFF_FIELD;
    }
  }
  return { limits, problems };
}

export const RACCT_OFF_FIELD = "Needs RACCT, which is off on this host (kern.racct.enable).";

/** Why the mlock option would be refused, or undefined. */
export function mlockProblem(mlock: boolean, limits: ContainerLimits, caps: ContainerCapabilities | undefined): string | undefined {
  if (!mlock) return undefined;
  if (caps && !caps.rctl) return "Locking memory needs RACCT, which is off on this host.";
  if (!limits.locked_memory) return "Locking memory needs a locked-memory limit.";
  return undefined;
}

// IPv4 addressing.

function ipv4ToInt(text: string): number | undefined {
  const parts = text.split(".");
  if (parts.length !== 4) return undefined;
  let n = 0;
  for (const p of parts) {
    if (!/^\d{1,3}$/.test(p)) return undefined;
    const v = Number(p);
    if (v > 255) return undefined;
    n = n * 256 + v;
  }
  return n;
}

/** Parses "a.b.c.d/len" into the address and prefix length. */
export function parseCIDR(text: string): { addr: number; len: number } | undefined {
  const m = /^([\d.]+)\/(\d{1,2})$/.exec(text.trim());
  if (!m) return undefined;
  const addr = ipv4ToInt(m[1] ?? "");
  const len = Number(m[2]);
  if (addr === undefined || len < 1 || len > 32) return undefined;
  return { addr, len };
}

function samePrefix(a: number, b: number, len: number): boolean {
  const size = 2 ** (32 - len);
  return Math.floor(a / size) === Math.floor(b / size);
}

/** What a user types for one NIC. */
export interface NICForm {
  bridge: string;
  mode: "dhcp" | "static";
  address: string;
  gateway: string;
}

export function newNIC(bridge: string): NICForm {
  return { bridge, mode: "dhcp", address: "", gateway: "" };
}

/** Why a NIC's addressing would be refused, or undefined. */
export function nicProblem(nic: NICForm): string | undefined {
  if (nic.bridge === "") return "Choose a bridge.";
  if (nic.mode === "dhcp") return undefined;
  const cidr = parseCIDR(nic.address);
  if (!cidr) return "Give the address with its prefix length, such as 192.0.2.10/24.";
  if (nic.gateway.trim() === "") return undefined;
  const gw = ipv4ToInt(nic.gateway.trim());
  if (gw === undefined) return "The gateway is an IPv4 address, such as 192.0.2.1.";
  if (gw === cidr.addr) return "The gateway must be another address than the jail's.";
  if (!samePrefix(gw, cidr.addr, cidr.len)) return "The gateway must be in the address's prefix.";
  return undefined;
}

/** Problems that span NICs: one gateway per jail. */
export function nicsProblem(nics: NICForm[]): string | undefined {
  if (nics.length > MAX_NICS) return `A jail has at most ${MAX_NICS} network interfaces.`;
  const gateways = nics.filter((n) => n.mode === "static" && n.gateway.trim() !== "").length;
  if (gateways > 1) return "Only one interface may have a gateway: a jail has one default route.";
  return undefined;
}

export function toNICSpec(nic: NICForm): ContainerNICSpec {
  if (nic.mode === "dhcp") return { bridge: nic.bridge, ipv4: { mode: "dhcp" } };
  const gateway = nic.gateway.trim();
  return { bridge: nic.bridge, ipv4: { mode: "static", address: nic.address.trim(), ...(gateway ? { gateway } : {}) } };
}

/** Whether a nameserver list may be given: only when no NIC uses DHCP. */
export function nameserversAllowed(nics: NICForm[]): boolean {
  return nics.length > 0 && nics.every((n) => n.mode === "static");
}

/** Splits the nameserver field (commas or spaces) and checks each entry loosely. */
export function parseNameservers(text: string): { list: string[]; problem?: string } {
  const list = text
    .split(/[\s,]+/)
    .map((s) => s.trim())
    .filter(Boolean);
  if (list.length > MAX_NAMESERVERS) return { list, problem: `At most ${MAX_NAMESERVERS} nameservers.` };
  const isAddress = (s: string) => ipv4ToInt(s) !== undefined || (s.includes(":") && /^[0-9A-Fa-f:]+$/.test(s));
  const bad = list.find((s) => !isAddress(s));
  return bad ? { list, problem: `Not an IPv4 or IPv6 address: ${bad}.` } : { list };
}

/** Why an SSH key line would be refused, or undefined. The API parses it properly. */
export function sshKeyProblem(text: string): string | undefined {
  const t = text.trim();
  if (t === "") return undefined;
  if (/[\r\n]/.test(t)) return "One key only, on one line.";
  if (t.length > 16384) return "The key is too long.";
  if (t.split(/\s+/).length < 2 || !/^(ssh-|ecdsa-|sk-)/.test(t)) {
    return 'An OpenSSH public key line: "type base64 [comment]", such as ssh-ed25519 AAAA… user@host, without options.';
  }
  return undefined;
}

// Clones (ADR 0010 decision 3, amendments k and l).

/** The slots of a jail's NICs with a static address. */
export function staticSlots(c: Container): number[] {
  return c.nics.filter((n) => n.ipv4.mode === "static").map((n) => n.slot);
}

/**
 * Why a jail cannot be cloned at all, or undefined. A jail with a static
 * address that has started cannot be cloned in this release: the clone
 * needs new addressing, and Skep writes addressing only into a tree that
 * has never run code (amendment l, Q8).
 */
export function cloneBlocked(c: Container): string | undefined {
  if (!c.provisioned) return "The jail is not provisioned yet.";
  if (c.started_once && staticSlots(c).length > 0) return STARTED_STATIC_CLONE;
  return undefined;
}

export const STARTED_STATIC_CLONE =
  "This jail has a static address and has started, so it cannot be cloned. A clone would need new addressing, and Skep writes addressing only into a jail's tree before any of its code has run: after the first start the jail's own processes may have changed those files, so Skep no longer writes there. Clone it before its first start, or give it DHCP addressing.";

/** What ADR 0010 decision 3 requires the interface to say about a jail clone. */
export const CONTAINER_CLONE_NOTE =
  "A clone is the same guest as its source. Skep gives it a new ID, name, hostname, and MAC addresses, but its SSH host keys (/etc/ssh) and /etc/hostid are the source's. Regenerate the host keys inside the clone (for example rm /etc/ssh/ssh_host_* and restart sshd) before it shares a network with the source.";

// Usage.

/** The metrics of one jail, if the sample holds them. */
export function metricsFor(metrics: Metrics | undefined, id: string): ContainerMetrics | undefined {
  return metrics?.containers?.find((m) => m.id === id);
}

/** A limit as a number, for the usage meters. */
export function limitNumber(v: ContainerLimits[keyof ContainerLimits]): number | undefined {
  if (typeof v === "number" && v > 0) return v;
  return undefined;
}

/** The message for a container refusal, when the API's own is not the clearest. */
export function containerErrorHint(code: string): string | undefined {
  switch (code) {
    case "rctl_unavailable":
      return "RACCT is off on this host (kern.racct.enable), so only the disk quota can be set. An administrator enables it in loader.conf and reboots.";
    case "mlock_requires_memory_limit":
      return "Locking memory needs a locked-memory limit, and RACCT on.";
    case "container_ssh_unavailable":
      return "This template has no sshd. Choose a template built with FreeBSD-ssh, or leave the key out.";
    case "template_not_provisioned":
      return "The template is still being built, or its build failed.";
    case "release_unsupported":
      return "The template's release does not match the host's major.minor version.";
    case "bridge_not_allowed":
      return "The bridge is not in the agent's network.bridges.";
    case "container_not_stopped":
      return "Stop the jail first.";
    case "container_started":
      return "Skep writes into a jail's tree only before its first start.";
    case "container_address_in_use":
      return "A running jail on the same bridge holds this static address.";
    case "container_mac_invalid":
      return "A jail's MAC address must be unicast and locally administered (the second hex digit is 2, 6, A, or E, as in 02:…), and not all zeroes.";
    case "container_mac_in_use":
      return "The MAC address is already on the bridge (the host's, a member interface's, or a device the bridge has learnt) or used by another Skep guest. Give the interface another MAC address.";
    case "clone_addressing_required":
      return "Every interface with a static address needs new addressing for the clone.";
    case "epair_unavailable":
      return "The if_epair module is not loaded on the host; an administrator adds it to kld_list.";
    case "devfs_ruleset_conflict":
      return "devfs ruleset 6001 on the host holds rules Skep did not write; an administrator must remove or renumber them.";
    case "container_start_failed":
      return "The console log below shows what /etc/rc wrote.";
    case "snapshot_not_latest":
      return "Rolling back to an older snapshot destroys the newer ones, and must be asked for.";
    case "snapshot_has_clones":
      return "Linked clones depend on it; delete the clones first.";
    case "storage_locked":
      return "The encryption key of the jail's dataset is not loaded; an administrator loads it on the host.";
  }
  return undefined;
}

// Editing (PUT /containers/{id}).

/** A size as the form shows it: 2G, 512M, or bytes. */
export function sizeText(bytes: number): string {
  if (bytes > 0 && bytes % 2 ** 30 === 0) return `${bytes / 2 ** 30}G`;
  if (bytes > 0 && bytes % MIB === 0) return `${bytes / MIB}M`;
  return String(bytes);
}

/** A jail's current limits as the limit fields. */
export function limitsToForm(l: ContainerLimits): LimitsForm {
  const size = (v: ContainerLimits["memory"]) => (typeof v === "number" && v > 0 ? sizeText(v) : typeof v === "string" ? v : "");
  const count = (v: number | undefined) => (v ? String(v) : "");
  return {
    memory: size(l.memory),
    locked_memory: size(l.locked_memory),
    cpu_percent: count(l.cpu_percent),
    max_processes: count(l.max_processes),
    open_files: count(l.open_files),
    disk_quota: size(l.disk_quota),
  };
}

/** A jail's NIC as the NIC fields. */
export function nicToForm(n: Container["nics"][number]): NICForm {
  return { bridge: n.bridge, mode: n.ipv4.mode, address: n.ipv4.address ?? "", gateway: n.ipv4.gateway ?? "" };
}

/**
 * The NICs of an update to a jail that has started: only the bridge of an
 * existing slot may change, so each keeps its addressing exactly as
 * recorded, and its MAC (left out, the slot keeps it).
 */
export function startedNICSpecs(c: Container, bridges: string[]): ContainerNICSpec[] {
  return c.nics.map((n, i) => ({ bridge: bridges[i] ?? n.bridge, ipv4: { ...n.ipv4 } }));
}
