import type { ApiError } from "@/api/client";
import { Notice } from "@/components/ui/notice";

/** What to tell the user about an error (ADR 0008, decision 5). */
export function describeError(error: ApiError): { title: string; text: string } {
  if (error.isNetwork) {
    return { title: "skep-api cannot be reached", text: "Check that the service is running on the host." };
  }
  if (error.isAgentUnavailable) {
    return { title: "The host agent is not reachable", text: "skep-api answers, but skep-agent does not." };
  }
  switch (error.status) {
    case 403:
      return error.code === "forbidden" && error.details?.permission !== undefined
        ? { title: "Your role does not allow this", text: `It needs the permission ${String(error.details.permission)}.` }
        : { title: "The request was refused", text: error.message };
    case 404:
      return { title: "Not found", text: error.message };
    case 429:
      return { title: "Too many attempts", text: `Wait ${error.retryAfter ?? "a few"} seconds and try again.` };
    case 500:
      return { title: "Something went wrong on the server", text: "Give the request ID below to the administrator." };
  }
  if (error.status >= 500) {
    return { title: "The service is not available", text: error.message };
  }
  return { title: "The request failed", text: error.message };
}

/** Shows an API error. With `stale`, it sits above data that is still shown. */
export function ErrorNotice({
  error,
  onRetry,
  stale,
  className,
}: {
  error: ApiError;
  onRetry?: () => void;
  stale?: boolean;
  className?: string;
}) {
  const { title, text } = describeError(error);
  return (
    <Notice tone="bad" title={title} className={className ?? "mb-4"}>
      <p>
        {text}
        {stale ? " Showing the last data received." : ""}
      </p>
      <div className="mt-1 flex flex-wrap items-center gap-3 text-xs text-muted-foreground">
        <span className="font-mono">{error.code}</span>
        {error.requestId ? <span className="font-mono">request {error.requestId}</span> : null}
        {onRetry ? (
          <button type="button" className="underline underline-offset-2" onClick={onRetry}>
            Try again
          </button>
        ) : null}
      </div>
    </Notice>
  );
}

/** A short inline form error with the request ID. */
export function InlineError({ error }: { error: ApiError }) {
  const { title, text } = describeError(error);
  return (
    <Notice tone="bad" title={title}>
      <p>{text}</p>
      {error.requestId ? <p className="mt-1 font-mono text-xs text-muted-foreground">request {error.requestId}</p> : null}
    </Notice>
  );
}
