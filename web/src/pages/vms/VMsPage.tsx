import { useState } from "react";
import { Link, useNavigate } from "react-router";

import { api } from "@/api/client";
import { useQuery } from "@/api/hooks";
import { PERMISSIONS } from "@/api/types";
import { useSession } from "@/app/session";
import { useSystem } from "@/app/system";
import { ErrorNotice, PageHeader, RefreshButton } from "@/components/page";
import { VMStateBadge } from "@/components/status";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Notice } from "@/components/ui/notice";
import { Skeleton } from "@/components/ui/skeleton";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { formatBytes, formatDateTime } from "@/lib/format";

import { CreateVMDialog } from "./CreateVMDialog";

export function VMsPage() {
  const { can } = useSession();
  const system = useSystem();
  const navigate = useNavigate();
  const list = useQuery("vms", (o) => api.vms(o), { refreshMs: 3_000 });
  const [creating, setCreating] = useState(false);
  const bhyve = system.data?.bhyve;
  const items = list.data?.items ?? [];

  return (
    <>
      <PageHeader
        title="Virtual machines"
        description="bhyve guests on this node. Open one for its details, its power actions, and its serial console."
        actions={
          <>
            <RefreshButton onClick={list.reload} updatedAt={list.updatedAt} />
            {can(PERMISSIONS.vmWrite) ? <Button size="sm" onClick={() => setCreating(true)}>Create VM</Button> : null}
          </>
        }
      />
      {bhyve && !bhyve.available ? (
        <div className="mb-4">
          <Notice tone="warn" title="bhyve cannot run a VM on this host">
            {bhyve.reason}
          </Notice>
        </div>
      ) : null}
      {bhyve?.simulated ? (
        <div className="mb-4">
          <Notice tone="info" title="Simulated host">
            This is a development build: VMs are simulated in memory and nothing runs.
          </Notice>
        </div>
      ) : null}
      {list.error ? <ErrorNotice error={list.error} onRetry={list.reload} stale={list.data !== undefined} /> : null}
      <Card>
        <CardContent className="px-2 pt-2 pb-2">
          {list.loading ? (
            <div className="space-y-2 p-3" aria-busy="true">
              <Skeleton className="h-5 w-full" />
              <Skeleton className="h-5 w-2/3" />
            </div>
          ) : items.length === 0 ? (
            <p className="p-4 text-sm text-muted-foreground">No virtual machines yet.</p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Name</TableHead>
                  <TableHead>State</TableHead>
                  <TableHead>CPUs</TableHead>
                  <TableHead>Memory</TableHead>
                  <TableHead>Disks</TableHead>
                  <TableHead>Created</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {items.map((vm) => (
                  <TableRow key={vm.id}>
                    <TableCell>
                      <Link to={`/vms/${vm.id}`} className="font-medium hover:underline">
                        {vm.name}
                      </Link>
                    </TableCell>
                    <TableCell className="space-x-1.5">
                      <VMStateBadge state={vm.observed.state} />
                      {!vm.provisioned ? <Badge tone="info">provisioning</Badge> : null}
                      {vm.drift ? (
                        <Badge tone="warn" title={`Should be ${vm.desired_state}`}>
                          drift
                        </Badge>
                      ) : null}
                    </TableCell>
                    <TableCell className="tabular-nums">{vm.cpu_count}</TableCell>
                    <TableCell className="tabular-nums">{formatBytes(vm.memory_bytes)}</TableCell>
                    <TableCell className="text-xs">
                      {vm.disks
                        .filter((d) => d.bus !== "ahci-cd")
                        .map((d) => formatBytes(d.size_bytes ?? 0))
                        .join(", ")}
                    </TableCell>
                    <TableCell className="text-xs whitespace-nowrap">{formatDateTime(vm.created_at)}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>
      {creating ? (
        <CreateVMDialog
          onClose={() => setCreating(false)}
          onCreated={(c) => {
            setCreating(false);
            navigate(`/vms/${c.vm.id}`);
          }}
        />
      ) : null}
    </>
  );
}
