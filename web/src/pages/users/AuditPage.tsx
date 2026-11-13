import { useState } from "react";

import { api, toApiError, type ApiError } from "@/api/client";
import { useQuery } from "@/api/hooks";
import type { AuditEntry } from "@/api/types";
import { ErrorNotice } from "@/components/errors";
import { RefreshButton } from "@/components/page";
import { Badge, type Tone } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { formatDateTime } from "@/lib/format";

const PAGE_SIZE = 50;

const OUTCOME_TONE: Record<string, Tone> = { success: "ok", failure: "warn", denied: "bad" };

interface Older {
  items: AuditEntry[];
  cursor: string | undefined;
  loading: boolean;
  error: ApiError | undefined;
}

function Details({ details }: { details: Record<string, unknown> | undefined }) {
  if (!details) return null;
  const entries = Object.entries(details);
  if (entries.length === 0) return null;
  return (
    <span className="font-mono text-xs text-muted-foreground">
      {entries.map(([k, v]) => `${k}=${typeof v === "string" ? v : JSON.stringify(v)}`).join(" ")}
    </span>
  );
}

/** The access audit log of skep-api, newest first, paged (ADR 0008, decision 1). */
export function AuditPage() {
  // The first page refreshes by itself; older pages are appended on demand.
  const first = useQuery("audit", (o) => api.audit({ limit: PAGE_SIZE }, o), { refreshMs: 15_000 });
  const [older, setOlder] = useState<Older>({ items: [], cursor: undefined, loading: false, error: undefined });
  const firstItems = first.data?.items ?? [];
  const seen = new Set(firstItems.map((e) => e.id));
  const items = [...firstItems, ...older.items.filter((e) => !seen.has(e.id))];
  const nextCursor = older.items.length > 0 ? older.cursor : first.data?.next_cursor;

  const loadMore = () => {
    if (!nextCursor || older.loading) return;
    setOlder((o) => ({ ...o, loading: true, error: undefined }));
    api
      .audit({ limit: PAGE_SIZE, cursor: nextCursor })
      .then((page) =>
        setOlder((o) => ({ items: [...o.items, ...page.items], cursor: page.next_cursor, loading: false, error: undefined })),
      )
      .catch((err: unknown) => setOlder((o) => ({ ...o, loading: false, error: toApiError(err) })));
  };

  return (
    <>
      <div className="mb-4 flex flex-wrap items-center justify-between gap-3">
        <p className="text-sm text-muted-foreground">
          Logins, logouts, denials, and every change to accounts, as skep-api recorded them. The agent keeps its own
          audit log of operations on the host.
        </p>
        <RefreshButton onClick={first.reload} updatedAt={first.updatedAt} />
      </div>
      {first.error ? <ErrorNotice error={first.error} onRetry={first.reload} stale={first.data !== undefined} /> : null}
      <Card>
        <CardContent className="px-2 pt-2 pb-2">
          {first.loading ? (
            <div className="space-y-2 p-3" aria-busy="true">
              <Skeleton className="h-5 w-full" />
              <Skeleton className="h-5 w-full" />
            </div>
          ) : items.length === 0 ? (
            <p className="p-4 text-sm text-muted-foreground">No entries.</p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>When</TableHead>
                  <TableHead>Actor</TableHead>
                  <TableHead>Action</TableHead>
                  <TableHead>Outcome</TableHead>
                  <TableHead>Resource</TableHead>
                  <TableHead>Source</TableHead>
                  <TableHead>Details</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {items.map((e) => (
                  <TableRow key={e.id}>
                    <TableCell className="text-xs whitespace-nowrap">{formatDateTime(e.occurred_at)}</TableCell>
                    <TableCell className="text-xs">{e.actor_name}</TableCell>
                    <TableCell className="font-mono text-xs">{e.action}</TableCell>
                    <TableCell>
                      <Badge tone={OUTCOME_TONE[e.outcome] ?? "neutral"}>{e.outcome}</Badge>
                    </TableCell>
                    <TableCell className="text-xs">
                      {e.resource_type ? (
                        <span title={e.resource_id}>
                          {e.resource_type}{" "}
                          <span className="font-mono text-muted-foreground">{e.resource_id?.slice(0, 8)}</span>
                        </span>
                      ) : null}
                    </TableCell>
                    <TableCell className="font-mono text-xs">{e.source_ip}</TableCell>
                    <TableCell className="max-w-md break-all">
                      <Details details={e.details} />
                      {e.request_id ? (
                        <span className="ml-1 font-mono text-xs text-muted-foreground" title="request ID">
                          req={e.request_id}
                        </span>
                      ) : null}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>
      {older.error ? (
        <div className="mt-4">
          <ErrorNotice error={older.error} onRetry={loadMore} />
        </div>
      ) : null}
      {nextCursor ? (
        <div className="mt-4 flex justify-center">
          <Button variant="outline" size="sm" onClick={loadMore} disabled={older.loading}>
            {older.loading ? "Loading…" : "Load older entries"}
          </Button>
        </div>
      ) : null}
    </>
  );
}
