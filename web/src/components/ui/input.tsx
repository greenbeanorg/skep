import type { InputHTMLAttributes, LabelHTMLAttributes } from "react";

import { cn } from "@/lib/utils";

export function Input({ className, type, ...props }: InputHTMLAttributes<HTMLInputElement>) {
  return (
    <input
      type={type ?? "text"}
      className={cn(
        "flex h-9 w-full rounded-md border bg-surface px-3 py-1 text-sm shadow-xs transition-colors",
        "placeholder:text-muted-foreground focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none",
        "disabled:cursor-not-allowed disabled:opacity-50 aria-invalid:border-bad",
        className,
      )}
      {...props}
    />
  );
}

export function Label({ className, ...props }: LabelHTMLAttributes<HTMLLabelElement>) {
  return <label className={cn("text-sm font-medium", className)} {...props} />;
}

export function Select({ className, children, ...props }: React.SelectHTMLAttributes<HTMLSelectElement>) {
  return (
    <select
      className={cn(
        "flex h-9 w-full rounded-md border bg-surface px-3 py-1 text-sm shadow-xs",
        "focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none disabled:opacity-50",
        className,
      )}
      {...props}
    >
      {children}
    </select>
  );
}

/** A small block of help or error text under a field. */
export function FieldText({ className, error, ...props }: React.HTMLAttributes<HTMLParagraphElement> & { error?: boolean }) {
  return <p className={cn("text-xs", error ? "text-bad" : "text-muted-foreground", className)} {...props} />;
}
