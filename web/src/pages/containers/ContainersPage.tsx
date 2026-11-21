import { useState } from "react";
import { Link, useNavigate } from "react-router";

import { api } from "@/api/client";
import { useQuery } from "@/api/hooks";
import { PERMISSIONS, type Container } from "@/api/types";
import { useSession } from "@/app/session";
import { useSystem } from "@/app/system";
import { ErrorNotice, PageHeader, RefreshButton } from "@/components/page";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { formatDateTime } from "@/lib/format";

import { CONTAINER_STATE_TONE } from "./container";
import { CreateContainerDialog } from "./CreateContainerDialog";
import { RacctOffNotice } from "./LimitsFields";

export function ContainerStateBadge({ state }: { state: Container["state"] }) {
  return <Badge tone={CONTAINER_STATE_TONE[state] ?? "neutral"}>{state}</Badge>;
}

/** The addressing of a jail's NICs, short: "bridge0 DHCP", "bridge1 192.0.2.10/24". */
export function addressing(c: Container): string {
  if (c.nics.length === 0) return "none";
  return c.nics.map((n) => `${n.bridge} ${n.ipv4.mode === "dhcp" ? "DHCP" : (n.ipv4.address ?? "static")}`).join(", ");
}

export function ContainersPage() {
  const { can } = useSession();
  const system = useSystem();
  const navigate = useNavigate();
  const list = useQuery("containers", (o) => api.containers(o), { refreshMs: 3_000 });
  const [creating, setCreating] = useState(false);
  const caps = system.data?.containers;
  const items = list.data?.items ?? [];

  return (
    <>
      <PageHeader
        title="Containers"
        description="FreeBSD jails on this node, each with its own network stack, cloned from a template. Open one for its details, power actions, snapshots, and usage."
        actions={
          <>
            <RefreshButton onClick={list.reload} updatedAt={list.updatedAt} />
            {can(PERMISSIONS.containerWrite) ? (
              <Button size="sm" onClick={() => setCreating(true)}>
                Create container
              </Button>
            ) : null}
          </>
        }
      />
      {caps && !caps.rctl ? <RacctOffNotice className="mb-4" /> : null}
      {list.error ? <ErrorNotice error={list.error} onRetry={list.reload} stale={list.data !== undefined} /> : null}
      <Card>
        <CardContent className="px-2 pt-2 pb-2">
          {list.loading ? (
            <div className="space-y-2 p-3" aria-busy="true">
              <Skeleton className="h-5 w-full" />
              <Skeleton className="h-5 w-2/3" />
            </div>
          ) : items.length === 0 ? (
            <p className="p-4 text-sm text-muted-foreground">No containers yet.</p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Name</TableHead>
                  <TableHead>State</TableHead>
                  <TableHead>Template</TableHead>
                  <TableHead>Network</TableHead>
                  <TableHead>Created</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {items.map((c) => (
                  <TableRow key={c.id}>
                    <TableCell>
                      <Link to={`/containers/${c.id}`} className="font-medium hover:underline">
                        {c.name}
                      </Link>
                      {c.hostname !== c.name ? <div className="text-xs text-muted-foreground">{c.hostname}</div> : null}
                    </TableCell>
                    <TableCell className="space-x-1.5">
                      <ContainerStateBadge state={c.state} />
                      {!c.provisioned ? <Badge tone="info">provisioning</Badge> : null}
                      {c.cloned_from_snapshot_id ? <Badge tone="neutral">clone</Badge> : null}
                    </TableCell>
                    <TableCell className="text-xs">
                      {c.template_name}
                      {c.release ? <span className="text-muted-foreground"> {c.release}</span> : null}
                    </TableCell>
                    <TableCell className="text-xs">{addressing(c)}</TableCell>
                    <TableCell className="text-xs whitespace-nowrap">{formatDateTime(c.created_at)}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>
      {creating ? (
        <CreateContainerDialog
          onClose={() => setCreating(false)}
          onCreated={(c) => {
            setCreating(false);
            navigate(`/containers/${c.container.id}`, { state: { task: c.task } });
          }}
        />
      ) : null}
    </>
  );
}
