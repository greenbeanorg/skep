import { Component, Suspense, type ReactNode } from "react";

import { Notice } from "@/components/ui/notice";
import { Skeleton } from "@/components/ui/skeleton";

/** What a page shows while its code is being fetched. */
export function PageLoading() {
  return (
    <div aria-busy="true" aria-label="Loading the page" className="space-y-3">
      <Skeleton className="h-7 w-48" />
      <Skeleton className="h-32 w-full" />
    </div>
  );
}

interface CatcherProps {
  resetKey: string;
  children: ReactNode;
}

interface CatcherState {
  failed: boolean;
}

/**
 * Catches a page whose code could not be loaded. The usual cause is an
 * upgrade of skep-api while the tab was open: the page files are named by
 * their content, so the old names are gone. Reloading fetches the current
 * interface. A new resetKey (the next navigation) clears the error.
 */
class LoadErrorCatcher extends Component<CatcherProps, CatcherState> {
  state: CatcherState = { failed: false };

  static getDerivedStateFromError(): CatcherState {
    return { failed: true };
  }

  componentDidUpdate(prev: CatcherProps) {
    if (prev.resetKey !== this.props.resetKey && this.state.failed) {
      this.setState({ failed: false });
    }
  }

  render() {
    if (this.state.failed) {
      return (
        <Notice tone="bad" title="This page could not be loaded">
          <p>skep-api may have been upgraded since this tab was opened. Reload to get the current interface.</p>
          <button type="button" className="mt-1 text-xs underline underline-offset-2" onClick={() => window.location.reload()}>
            Reload
          </button>
        </Notice>
      );
    }
    return this.props.children;
  }
}

/**
 * Wraps the routed page: a placeholder while a page's code loads (pages are
 * split by route), and a reload prompt if it cannot be loaded.
 */
export function PageBoundary({ resetKey, children }: CatcherProps) {
  return (
    <LoadErrorCatcher resetKey={resetKey}>
      <Suspense fallback={<PageLoading />}>{children}</Suspense>
    </LoadErrorCatcher>
  );
}
