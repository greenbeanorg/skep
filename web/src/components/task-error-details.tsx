import type { Task } from "@/api/types";

type Details = NonNullable<Task["error"]>["details"];

/** Renders one detail value as text: arrays joined, objects as JSON. */
function detailText(v: unknown): string {
  if (v === null || v === undefined) return "";
  if (Array.isArray(v)) return v.map(detailText).join(", ");
  if (typeof v === "object") return JSON.stringify(v);
  return String(v);
}

/**
 * The structured details the agent recorded with a task's failure (for
 * disk_device_missing: dataset, reason, command), sorted by key. The API
 * sends only keys reviewed for each error code; React escapes the text.
 */
export function TaskErrorDetails({
  details,
  className,
}: {
  details: Details;
  className?: string;
}) {
  const entries = Object.entries(details ?? {}).sort(([a], [b]) =>
    a.localeCompare(b),
  );
  if (entries.length === 0) return null;
  return (
    <dl
      className={`mt-1 grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5 text-xs ${className ?? ""}`}
      data-testid="task-error-details"
    >
      {entries.map(([k, v]) => (
        <div key={k} className="contents">
          <dt className="font-mono text-muted-foreground">{k}</dt>
          <dd className="font-mono break-all">{detailText(v)}</dd>
        </div>
      ))}
    </dl>
  );
}
