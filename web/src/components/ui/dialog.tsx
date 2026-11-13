import { useEffect, useId, useRef, type ReactNode } from "react";

import { cn } from "@/lib/utils";

export interface DialogProps {
  open: boolean;
  onClose: () => void;
  title: ReactNode;
  description?: ReactNode;
  children: ReactNode;
  className?: string;
}

/**
 * A modal dialog on the native <dialog> element: focus is trapped by the
 * browser, Escape closes it, and the backdrop is rendered by the browser
 * too. No inline style, as the CSP requires.
 */
export function Dialog({ open, onClose, title, description, children, className }: DialogProps) {
  const ref = useRef<HTMLDialogElement>(null);
  const titleId = useId();
  const descId = useId();

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    if (open && !el.open) {
      // jsdom has no showModal; fall back to the open attribute.
      if (typeof el.showModal === "function") el.showModal();
      else el.setAttribute("open", "");
    } else if (!open && el.open) {
      el.close();
    }
  }, [open]);

  return (
    <dialog
      ref={ref}
      aria-labelledby={titleId}
      aria-describedby={description ? descId : undefined}
      onClose={onClose}
      onCancel={(e) => {
        e.preventDefault();
        onClose();
      }}
      className={cn(
        "m-auto w-full max-w-md rounded-lg border bg-surface p-0 text-foreground shadow-xl",
        "backdrop:bg-black/40 open:flex open:flex-col",
        className,
      )}
    >
      <div className="px-5 pt-5 pb-2">
        <h2 id={titleId} className="text-base font-semibold tracking-tight">
          {title}
        </h2>
        {description ? (
          <p id={descId} className="mt-1 text-sm text-muted-foreground">
            {description}
          </p>
        ) : null}
      </div>
      <div className="px-5 pb-5">{children}</div>
    </dialog>
  );
}

/** The row of buttons at the bottom of a dialog. */
export function DialogActions({ children }: { children: ReactNode }) {
  return <div className="mt-5 flex flex-wrap justify-end gap-2">{children}</div>;
}
