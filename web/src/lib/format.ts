// Formatting helpers. Pure functions, no DOM.

const UNITS = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"] as const;

/** Formats a byte count with binary units, for example "1.5 GiB". */
export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return "unknown";
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < UNITS.length - 1) {
    value /= 1024;
    unit++;
  }
  const digits = unit === 0 || value >= 100 ? 0 : 1;
  return `${value.toFixed(digits)} ${UNITS[unit]}`;
}

/** Formats a number of seconds as "3d 4h", "5h 12m", "7m 3s", or "42s". */
export function formatDuration(totalSeconds: number): string {
  if (!Number.isFinite(totalSeconds) || totalSeconds < 0) return "unknown";
  const s = Math.floor(totalSeconds);
  const days = Math.floor(s / 86400);
  const hours = Math.floor((s % 86400) / 3600);
  const minutes = Math.floor((s % 3600) / 60);
  const seconds = s % 60;
  const pair = (big: string, small: string, smallValue: number) => (smallValue > 0 ? `${big} ${small}` : big);
  if (days > 0) return pair(`${days}d`, `${hours}h`, hours);
  if (hours > 0) return pair(`${hours}h`, `${minutes}m`, minutes);
  if (minutes > 0) return pair(`${minutes}m`, `${seconds}s`, seconds);
  return `${seconds}s`;
}

/** Formats the time between two ISO timestamps, with milliseconds when short. */
export function formatElapsed(startIso: string | undefined, endIso: string | undefined): string {
  if (!startIso || !endIso) return "";
  const ms = Date.parse(endIso) - Date.parse(startIso);
  if (!Number.isFinite(ms) || ms < 0) return "";
  if (ms < 1000) return `${ms} ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)} s`;
  return formatDuration(ms / 1000);
}

/** Formats an ISO timestamp in the viewer's locale and time zone. */
export function formatDateTime(iso: string | undefined): string {
  if (!iso) return "";
  const t = Date.parse(iso);
  if (!Number.isFinite(t)) return iso;
  return new Date(t).toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

/** Formats how long ago an ISO timestamp was, for example "5m ago". */
export function formatAgo(iso: string | undefined, now: number = Date.now()): string {
  if (!iso) return "";
  const t = Date.parse(iso);
  if (!Number.isFinite(t)) return "";
  const seconds = Math.max(0, Math.floor((now - t) / 1000));
  if (seconds < 5) return "just now";
  return `${formatDuration(seconds)} ago`;
}

/** Returns used/total as a percentage from 0 to 100, or null when unknown. */
export function percent(used: number, total: number): number | null {
  if (!Number.isFinite(used) || !Number.isFinite(total) || total <= 0) return null;
  return Math.min(100, Math.max(0, (used / total) * 100));
}

/** Shortens a UUID for display. The full value belongs in a title attribute. */
export function shortId(id: string): string {
  return id.length > 13 ? id.slice(0, 8) : id;
}
