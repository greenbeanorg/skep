import { useState } from "react";
import { Link } from "react-router";

import { api, toApiError, type ApiError } from "@/api/client";
import { useQuery } from "@/api/hooks";
import { TASK_STATUSES, type Task, type TaskStatus } from "@/api/types";
import { ErrorNotice, PageHeader, RefreshButton } from "@/components/page";
import { TaskStatusBadge } from "@/components/status";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { formatDateTime, formatElapsed, shortId } from "@/lib/format";
import { cn } from "@/lib/utils";

const PAGE_SIZE = 50;

type Filter = TaskStatus | "all";

interface Older {
  /** The filter these pages belong to. Pages of another filter are ignored. */
  filter: Filter;
  items: Task[];
  cursor: string | undefined;
  loading: boolean;
  error: ApiError | undefined;
}

function noOlder(filter: Filter): Older {
  return { filter, items: [], cursor: undefined, loading: false, error: undefined };
}

export function TasksPage() {
  const [filter, setFilter] = useState<Filter>("all");
  const status = filter === "all" ? undefined : filter;

  // The first page refreshes by itself. Older pages are appended on demand
  // and are not refreshed: finished tasks do not change.
  const first = useQuery(`tasks:${filter}`, (o) => api.tasks({ status, limit: PAGE_SIZE }, o), {
    refreshMs: 5_000,
  });
  const [olderState, setOlder] = useState<Older>(() => noOlder("all"));
  const older = olderState.filter === filter ? olderState : noOlder(filter);

  const firstItems = first.data?.items ?? [];
  const seen = new Set(firstItems.map((t) => t.id));
  const items = [...firstItems, ...older.items.filter((t) => !seen.has(t.id))];
  const nextCursor = older.items.length > 0 ? older.cursor : first.data?.next_cursor;

  const loadMore = () => {
    if (!nextCursor || older.loading) return;
    const forFilter = filter;
    setOlder({ ...older, loading: true, error: undefined });
    api
      .tasks({ status, limit: PAGE_SIZE, cursor: nextCursor })
      .then((page) => {
        // A page that arrives after the filter changed belongs to a list
        // that is no longer shown.
        setOlder((prev) =>
          prev.filter === forFilter
            ? {
                filter: forFilter,
                items: [...prev.items, ...page.items],
                cursor: page.next_cursor,
                loading: false,
                error: undefined,
              }
            : prev,
        );
      })
      .catch((err: unknown) => {
        const error = toApiError(err);
        setOlder((prev) => (prev.filter === forFilter ? { ...prev, loading: false, error } : prev));
      });
  };

  return (
    <>
      <PageHeader
        title="Tasks"
        description="Every operation of the agent runs as a task. Tasks cannot be submitted or cancelled here yet."
        actions={<RefreshButton onClick={first.reload} updatedAt={first.updatedAt} />}
      />

      <div className="mb-4 flex flex-wrap gap-1.5" role="group" aria-label="Filter by status">
        {(["all", ...TASK_STATUSES] as Filter[]).map((f) => (
          <button
            key={f}
            type="button"
            aria-pressed={filter === f}
            onClick={() => setFilter(f)}
            className={cn(
              "rounded-full border px-3 py-1 text-xs font-medium transition-colors",
              filter === f ? "border-transparent bg-accent text-accent-foreground" : "bg-surface hover:bg-muted",
            )}
          >
            {f}
          </button>
        ))}
      </div>

      {first.error ? <ErrorNotice error={first.error} onRetry={first.reload} stale={first.data !== undefined} /> : null}

      <Card>
        <CardContent className="px-2 pt-2 pb-2">
          {first.loading ? (
            <div className="space-y-2 p-3" aria-busy="true">
              <Skeleton className="h-5 w-full" />
              <Skeleton className="h-5 w-full" />
              <Skeleton className="h-5 w-2/3" />
            </div>
          ) : items.length === 0 ? (
            <p className="p-4 text-sm text-muted-foreground">
              {first.error ? "No tasks to show." : filter === "all" ? "No tasks yet." : `No ${filter} tasks.`}
            </p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Task</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Resource</TableHead>
                  <TableHead>Requested by</TableHead>
                  <TableHead>Created</TableHead>
                  <TableHead className="text-right">Run time</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {items.map((t) => (
                  <TableRow key={t.id}>
                    <TableCell>
                      <Link to={`/tasks/${t.id}`} className="hover:underline" title={t.id}>
                        <span className="font-mono text-[0.8125rem]">{t.type}</span>
                        <span className="ml-2 font-mono text-xs text-muted-foreground">{shortId(t.id)}</span>
                      </Link>
                    </TableCell>
                    <TableCell>
                      <TaskStatusBadge status={t.status} />
                      {t.error ? <span className="ml-2 font-mono text-xs text-muted-foreground">{t.error.code}</span> : null}
                    </TableCell>
                    <TableCell className="text-xs">
                      {t.resource_type ? (
                        <span title={t.resource_id}>
                          {t.resource_type}{" "}
                          <span className="font-mono text-muted-foreground">{shortId(t.resource_id ?? "")}</span>
                        </span>
                      ) : (
                        <span className="text-muted-foreground">none</span>
                      )}
                    </TableCell>
                    <TableCell className="text-xs">{t.actor.name}</TableCell>
                    <TableCell className="text-xs whitespace-nowrap">{formatDateTime(t.created_at)}</TableCell>
                    <TableCell className="text-right text-xs tabular-nums">
                      {formatElapsed(t.started_at, t.finished_at)}
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
            {older.loading ? "Loading…" : "Load older tasks"}
          </Button>
        </div>
      ) : null}
    </>
  );
}
