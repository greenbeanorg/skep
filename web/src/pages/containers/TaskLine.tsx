import { useEffect, useRef } from "react";
import { Link } from "react-router";

import { api } from "@/api/client";
import { useQuery } from "@/api/hooks";
import type { Task } from "@/api/types";
import { TaskStatusBadge } from "@/components/status";

import { containerErrorHint } from "./container";

const DONE = new Set(["succeeded", "failed", "cancelled", "interrupted"]);

/**
 * The task of the last write from a container page, followed until it
 * ends: its status, its latest progress message while it runs, and the
 * error code, message, and a hint when it did not succeed.
 */
export function TaskLine({ task, onDone }: { task: Task; onDone?: () => void }) {
  const live = useQuery(`task:${task.id}`, (o) => api.task(task.id, o), { refreshMs: 1_000 });
  const t = live.data ?? task;
  const done = DONE.has(t.status);
  const last = live.data?.events?.at(-1);
  // Tell the page once, when the live task has ended, so it reloads.
  const notified = useRef(false);
  const ended = done && live.data !== undefined;
  useEffect(() => {
    if (ended && !notified.current) {
      notified.current = true;
      onDone?.();
    }
  }, [ended, onDone]);
  const hint = t.error ? containerErrorHint(t.error.code) : undefined;
  return (
    <div className="space-y-1 text-sm" role="status" data-testid="task-line">
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-mono text-[0.8125rem]">{t.type}</span>
        <TaskStatusBadge status={t.status} />
        {!done && last ? <span className="text-muted-foreground">{last.message}</span> : null}
        <Link to={`/tasks/${t.id}`} className="text-xs text-muted-foreground hover:underline">
          task details
        </Link>
      </div>
      {t.error ? (
        <div className="rounded-md border border-bad/40 bg-bad-soft px-3 py-2 text-[0.8125rem]">
          <span className="font-mono">{t.error.code}</span>: {t.error.message}
          {hint ? <span className="block text-muted-foreground">{hint}</span> : null}
        </div>
      ) : null}
    </div>
  );
}
