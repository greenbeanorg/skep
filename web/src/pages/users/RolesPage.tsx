import { api } from "@/api/client";
import { useQuery } from "@/api/hooks";
import { ErrorNotice } from "@/components/errors";
import { LoadingCards } from "@/components/page";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";

/** Roles and their permissions, read-only (ADR 0006, decision 3). */
export function RolesPage() {
  const roles = useQuery("roles", (o) => api.roles(o));
  const items = roles.data?.items ?? [];
  return (
    <>
      <p className="mb-4 text-sm text-muted-foreground">
        A role is a set of permissions of the form <span className="font-mono">resource:action</span>; an asterisk means
        every permission. Roles cannot be edited in this release.
      </p>
      {roles.error ? <ErrorNotice error={roles.error} onRetry={roles.reload} stale={roles.data !== undefined} /> : null}
      {roles.loading ? <LoadingCards count={3} /> : null}
      <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
        {items.map((r) => (
          <Card key={r.id}>
            <CardHeader>
              <div>
                <CardTitle>{r.name}</CardTitle>
                <CardDescription>{r.description}</CardDescription>
              </div>
              {r.built_in ? <Badge tone="neutral">built-in</Badge> : null}
            </CardHeader>
            <CardContent>
              {r.permissions.includes("*") ? (
                <p className="text-sm">Every permission.</p>
              ) : r.permissions.length === 0 ? (
                <p className="text-sm text-muted-foreground">No permissions.</p>
              ) : (
                <ul className="flex flex-wrap gap-1.5">
                  {r.permissions.map((p) => (
                    <li key={p}>
                      <Badge tone="brand" className="font-mono">
                        {p}
                      </Badge>
                    </li>
                  ))}
                </ul>
              )}
            </CardContent>
          </Card>
        ))}
      </div>
    </>
  );
}
