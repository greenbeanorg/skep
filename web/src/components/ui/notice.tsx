import { cva, type VariantProps } from "class-variance-authority";
import type { HTMLAttributes, ReactNode } from "react";

import { cn } from "@/lib/utils";

const noticeVariants = cva("rounded-lg border px-4 py-3 text-sm", {
  variants: {
    tone: {
      neutral: "border-border bg-muted",
      info: "border-info/40 bg-info-soft",
      warn: "border-warn/40 bg-warn-soft",
      bad: "border-bad/40 bg-bad-soft",
      ok: "border-ok/40 bg-ok-soft",
    },
  },
  defaultVariants: { tone: "neutral" },
});

const titleTone: Record<NonNullable<VariantProps<typeof noticeVariants>["tone"]>, string> = {
  neutral: "text-foreground",
  info: "text-info",
  warn: "text-warn",
  bad: "text-bad",
  ok: "text-ok",
};

export interface NoticeProps extends Omit<HTMLAttributes<HTMLDivElement>, "title">, VariantProps<typeof noticeVariants> {
  title?: ReactNode;
}

/** A boxed message: a banner, a form error, a confirmation's warning. */
export function Notice({ className, tone, title, children, role, ...props }: NoticeProps) {
  const t = tone ?? "neutral";
  return (
    <div role={role ?? (t === "bad" || t === "warn" ? "alert" : "status")} className={cn(noticeVariants({ tone }), className)} {...props}>
      {title ? <div className={cn("font-medium", titleTone[t])}>{title}</div> : null}
      {children ? <div className={cn(title ? "mt-0.5" : "", "text-foreground/85")}>{children}</div> : null}
    </div>
  );
}
