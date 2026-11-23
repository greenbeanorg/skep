import { useState } from "react";
import { Link, useParams } from "react-router";

import { api, toApiError, type ApiError } from "@/api/client";
import { useQuery } from "@/api/hooks";
import { PERMISSIONS, type TaskDetail, type TaskStatus } from "@/api/types";
import { useSession } from "@/app/session";
import { ChevronLeftIcon } from "@/components/icons";
import { ErrorNotice, Fact, Facts, LoadingCards, PageHeader, RefreshButton } from "@/components/page";
import { TaskStatusBadge } from "@/components/status";
import { TaskErrorDetails } from "@/components/task-error-details";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Notice } from "@/components/ui/notice";
import { formatDateTime, formatElapsed, shortId } from "@/lib/format";
import { ConfirmDialog } from "@/pages/users/dialogs";

function isFinished(status: TaskStatus): boolean {
  return status !== "pending" && status !== "running";
}

function Json({ value }: { value: unknown }) {
  return (
    <pre className="overflow-x-auto rounded-md bg-muted p-3 font-mono text-xs leading-relaxed">
      {JSON.stringify(value, null, 2)}
    </pre>
  );
}

function TaskView({ task }: { task: TaskDetail }) {
  const events = task.events ?? [];
  return (
    <div className="grid gap-4 lg:grid-cols-2">
      <Card>
        <CardHeader>
          <div>
            <CardTitle>Request</CardTitle>
            <CardDescription>What was asked, and by whom</CardDescription>
          </div>
        </CardHeader>
        <CardContent>
          <Facts>
            <Fact label="Task ID" mono>
              {task.id}
            </Fact>
            <Fact label="Type" mono>
              {task.type}
            </Fact>
            <Fact label="Resource">
              {task.resource_type ? (
                <>
                  {task.resource_type} <span className="font-mono text-[0.8125rem]">{task.resource_id}</span>
                </>
              ) : (
                "none"
              )}
            </Fact>
            <Fact label="Requested by">
              {task.actor.name}
              {task.actor.id !== task.actor.name ? (
                <span className="ml-2 font-mono text-xs text-muted-foreground">{task.actor.id}</span>
              ) : null}
            </Fact>
            {task.request_id ? (
              <Fact label="Request ID" mono>
                {task.request_id}
              </Fact>
            ) : null}
            <Fact label="Cancellable">{task.cancellable ? "yes, while running" : "not while running"}</Fact>
          </Facts>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <div>
            <CardTitle>Outcome</CardTitle>
            <CardDescription>
              {isFinished(task.status) ? "This task is finished and will not change." : "This task is not finished."}
            </CardDescription>
          </div>
          <TaskStatusBadge status={task.status} />
        </CardHeader>
        <CardContent>
          <Facts>
            <Fact label="Created">{formatDateTime(task.created_at)}</Fact>
            <Fact label="Started">{formatDateTime(task.started_at) || "not started"}</Fact>
            <Fact label="Finished">{formatDateTime(task.finished_at) || "not finished"}</Fact>
            <Fact label="Run time">{formatElapsed(task.started_at, task.finished_at) || "n/a"}</Fact>
            {task.cancel_requested_at ? (
              <Fact label="Cancel requested">{formatDateTime(task.cancel_requested_at)}</Fact>
            ) : null}
          </Facts>
          {task.error ? (
            <div className="mt-4 rounded-md border border-bad/40 bg-bad-soft px-3 py-2 text-sm">
              <div className="font-mono text-xs text-bad">{task.error.code}</div>
              <div className="mt-0.5">{task.error.message}</div>
              <TaskErrorDetails details={task.error.details} />
            </div>
          ) : null}
          {task.status === "interrupted" ? (
            <p className="mt-3 text-xs text-muted-foreground">
              Skep never resumes an interrupted task. Check the resource, then submit a new task if needed.
            </p>
          ) : null}
        </CardContent>
      </Card>

      {task.result !== undefined && task.result !== null ? (
        <Card className="lg:col-span-2">
          <CardHeader>
            <CardTitle>Result</CardTitle>
          </CardHeader>
          <CardContent>
            <Json value={task.result} />
          </CardContent>
        </Card>
      ) : null}

      <Card className="lg:col-span-2">
        <CardHeader>
          <div>
            <CardTitle>Events</CardTitle>
            <CardDescription>State changes and progress, oldest first</CardDescription>
          </div>
        </CardHeader>
        <CardContent>
          {events.length === 0 ? (
            <p className="text-sm text-muted-foreground">No events.</p>
          ) : (
            <ol className="space-y-3 border-l pl-4">
              {events.map((e) => (
                <li key={e.id} className="relative text-sm">
                  <span
                    className={
                      "absolute top-1.5 -left-[1.3rem] size-2 rounded-full " +
                      (e.kind === "state" ? "bg-primary" : "bg-muted-foreground/50")
                    }
                    aria-hidden="true"
                  />
                  <div className="flex flex-wrap items-baseline gap-x-2">
                    <span className={e.kind === "state" ? "font-medium" : ""}>{e.message}</span>
                    {e.level !== "info" ? <Badge tone={e.level === "error" ? "bad" : "warn"}>{e.level}</Badge> : null}
                    <span className="text-xs text-muted-foreground">{formatDateTime(e.created_at)}</span>
                  </div>
                  {e.data !== undefined && e.data !== null ? (
                    <div className="mt-1">
                      <Json value={e.data} />
                    </div>
                  ) : null}
                </li>
              ))}
            </ol>
          )}
        </CardContent>
      </Card>
    </div>
  );
}

/** Whether the cancel button is offered: the role allows it and the task can still be cancelled. */
export function canCancel(task: TaskDetail, can: (p: typeof PERMISSIONS.taskCancel) => boolean): boolean {
  return can(PERMISSIONS.taskCancel) && !isFinished(task.status) && task.cancellable && !task.cancel_requested_at;
}

/** The cancel confirmation names the task (ADR 0008, decision 7). */
function CancelTask({ task, onDone }: { task: TaskDetail; onDone: () => void }) {
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<ApiError | undefined>();
  const confirm = async () => {
    setBusy(true);
    setError(undefined);
    try {
      await api.cancelTask(task.id);
      setOpen(false);
      onDone();
    } catch (err) {
      const apiErr = toApiError(err);
      if (apiErr.code === "not_cancellable") {
        Object.assign(apiErr, { message: "The task can no longer be cancelled; it finished or is finishing." });
      }
      setError(apiErr);
    } finally {
      setBusy(false);
    }
  };
  return (
    <>
      <Button variant="outline" size="sm" onClick={() => setOpen(true)}>
        Cancel task
      </Button>
      {open ? (
        <ConfirmDialog
          title={`Cancel ${task.type} ${shortId(task.id)}?`}
          description="The agent asks the task to stop. What it had already done is not undone."
          confirmLabel="Cancel task"
          destructive
          busy={busy}
          error={error}
          onConfirm={() => void confirm()}
          onClose={() => setOpen(false)}
        >
          <Facts>
            <Fact label="Task ID" mono>
              {task.id}
            </Fact>
            <Fact label="Type" mono>
              {task.type}
            </Fact>
            <Fact label="Requested by">{task.actor.name}</Fact>
          </Facts>
        </ConfirmDialog>
      ) : null}
    </>
  );
}

export function TaskDetailPage() {
  const { id = "" } = useParams();
  const { can } = useSession();
  const query = useQuery(id ? `task:${id}` : null, (o) => api.task(id, o), { refreshMs: 3_000 });
  const task = query.data;

  return (
    <>
      <Link to="/tasks" className="mb-3 inline-flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground">
        <ChevronLeftIcon className="size-3.5" />
        All tasks
      </Link>
      <PageHeader
        title={task ? task.type : "Task"}
        description={task ? undefined : <span className="font-mono">{id}</span>}
        actions={
          <>
            {task && canCancel(task, can) ? <CancelTask task={task} onDone={query.reload} /> : null}
            <RefreshButton onClick={query.reload} updatedAt={query.updatedAt} />
          </>
        }
      />
      {query.error ? <ErrorNotice error={query.error} onRetry={query.reload} stale={task !== undefined} /> : null}
      {task?.cancel_requested_at && !isFinished(task.status) ? (
        <Notice tone="info" className="mb-4" title="Cancellation requested">
          Asked at {formatDateTime(task.cancel_requested_at)}. The task stops when it next checks.
        </Notice>
      ) : null}
      {query.loading ? <LoadingCards count={2} /> : null}
      {task ? <TaskView task={task} /> : null}
    </>
  );
}
