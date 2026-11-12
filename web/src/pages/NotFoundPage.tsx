import { Link } from "react-router";

import { PageHeader } from "@/components/page";

export function NotFoundPage() {
  return (
    <>
      <PageHeader title="Page not found" description="There is nothing at this address." />
      <Link to="/" className="text-sm font-medium underline underline-offset-2">
        Go to the dashboard
      </Link>
    </>
  );
}
