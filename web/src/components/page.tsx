import type { ReactNode } from "react";

import { RefreshIcon } from "@/components/icons";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { formatAgo } from "@/lib/format";
import { cn } from "@/lib/utils";

export { ErrorNotice } from "@/components/errors";

export function PageHeader({
  title,
  description,
  actions,
}: {
  title: string;
  description?: ReactNode;
  actions?: ReactNode;
}) {
  return (
    <div className="mb-6 flex flex-wrap items-end justify-between gap-3">
      <div>
        <h1 className="text-xl font-semibold tracking-tight">{title}</h1>
        {description ? <p className="mt-1 text-sm text-muted-foreground">{description}</p> : null}
      </div>
      {actions ? <div className="flex items-center gap-2">{actions}</div> : null}
    </div>
  );
}

export function RefreshButton({ onClick, updatedAt }: { onClick: () => void; updatedAt?: number }) {
  return (
    <>
      {updatedAt ? (
        <span className="text-xs text-muted-foreground">updated {formatAgo(new Date(updatedAt).toISOString())}</span>
      ) : null}
      <Button variant="outline" size="sm" onClick={onClick}>
        <RefreshIcon />
        Refresh
      </Button>
    </>
  );
}

export function LoadingCards({ count = 3 }: { count?: number }) {
  return (
    <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3" aria-busy="true" aria-label="Loading">
      {Array.from({ length: count }, (_, i) => (
        <Card key={i}>
          <CardContent className="space-y-3 pt-5">
            <Skeleton className="h-4 w-1/3" />
            <Skeleton className="h-4 w-2/3" />
            <Skeleton className="h-4 w-1/2" />
          </CardContent>
        </Card>
      ))}
    </div>
  );
}

/** A definition list for label and value pairs. */
export function Facts({ children, className }: { children: ReactNode; className?: string }) {
  return <dl className={cn("grid grid-cols-[minmax(7rem,auto)_1fr] gap-x-4 gap-y-2 text-sm", className)}>{children}</dl>;
}

export function Fact({ label, children, mono }: { label: string; children: ReactNode; mono?: boolean }) {
  return (
    <>
      <dt className="text-muted-foreground">{label}</dt>
      <dd className={cn("min-w-0 break-words", mono && "font-mono text-[0.8125rem]")}>{children}</dd>
    </>
  );
}

/** A horizontal usage bar. `value` is a percentage, or null when unknown. */
export function Meter({ value, label }: { value: number | null; label: string }) {
  const tone = value === null ? "bg-muted-foreground" : value >= 90 ? "bg-bad" : value >= 75 ? "bg-warn" : "bg-primary";
  return (
    <div
      className="h-2 w-full overflow-hidden rounded-full bg-muted"
      role="meter"
      aria-label={label}
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={value === null ? undefined : Math.round(value)}
    >
      <div className={cn("h-full rounded-full", tone)} style={{ width: `${value ?? 0}%` }} />
    </div>
  );
}
