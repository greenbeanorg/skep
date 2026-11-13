import { Link } from "react-router";

import { PageHeader } from "@/components/page";
import { Notice } from "@/components/ui/notice";

/** Shown instead of a page the role does not allow (ADR 0008, decision 5). */
export function ForbiddenPage({ permission }: { permission: string }) {
  return (
    <>
      <PageHeader title="Not allowed" />
      <Notice tone="warn" title="Your role does not allow this">
        <p>
          This page needs the permission <span className="font-mono">{permission}</span>. The server enforces the
          same rule; this page only saves you the request.
        </p>
        <p className="mt-2">
          <Link to="/" className="font-medium underline underline-offset-2">
            Go to the dashboard
          </Link>
        </p>
      </Notice>
    </>
  );
}
