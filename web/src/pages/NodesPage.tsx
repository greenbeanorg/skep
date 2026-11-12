import { api } from "@/api/client";
import { useQuery } from "@/api/hooks";
import type { Hardware } from "@/api/types";
import { ErrorNotice, Fact, Facts, LoadingCards, Meter, PageHeader, RefreshButton } from "@/components/page";
import { CapabilityBadge, PoolHealthBadge } from "@/components/status";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { formatBytes, formatDateTime, percent } from "@/lib/format";

function yesNo(v: boolean): string {
  return v ? "yes" : "no";
}

function HardwareView({ hw }: { hw: Hardware }) {
  const used = hw.memory.total_bytes - hw.memory.available_bytes;
  const usedPct = percent(used, hw.memory.total_bytes);
  const { bhyve, qemu } = hw.hypervisors;
  const zfs = hw.storage.zfs;
  const pools = zfs.pools ?? [];
  const binaries = qemu.binaries ?? [];
  const warnings = hw.warnings ?? [];

  return (
    <div className="grid gap-4 lg:grid-cols-2">
      {warnings.length > 0 ? (
        <div className="rounded-lg border border-warn/40 bg-warn-soft px-4 py-3 text-sm lg:col-span-2">
          <div className="font-medium text-warn">Discovery warnings</div>
          <ul className="mt-1 list-disc pl-5 text-foreground/80">
            {warnings.map((w, i) => (
              <li key={i}>{w}</li>
            ))}
          </ul>
        </div>
      ) : null}

      <Card>
        <CardHeader>
          <div>
            <CardTitle>Processor</CardTitle>
            <CardDescription>{hw.host.architecture}</CardDescription>
          </div>
        </CardHeader>
        <CardContent>
          <Facts>
            <Fact label="Model">{hw.cpu.model || "unknown"}</Fact>
            <Fact label="Cores">{hw.cpu.cores}</Fact>
            <Fact label="Threads">{hw.cpu.threads}</Fact>
          </Facts>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <div>
            <CardTitle>Memory</CardTitle>
            <CardDescription>
              {formatBytes(used)} in use of {formatBytes(hw.memory.total_bytes)}
            </CardDescription>
          </div>
          <span className="text-sm font-medium tabular-nums">{usedPct === null ? "" : `${Math.round(usedPct)}%`}</span>
        </CardHeader>
        <CardContent>
          <Meter value={usedPct} label="Memory in use" />
          <Facts className="mt-4">
            <Fact label="Total">{formatBytes(hw.memory.total_bytes)}</Fact>
            <Fact label="Available">{formatBytes(hw.memory.available_bytes)}</Fact>
          </Facts>
          <p className="mt-3 text-xs text-muted-foreground">
            "Available" is the agent's estimate. On FreeBSD it does not count memory the ZFS ARC could give back.
          </p>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <div>
            <CardTitle>bhyve</CardTitle>
            <CardDescription>Primary hypervisor</CardDescription>
          </div>
          <CapabilityBadge capability={bhyve} />
        </CardHeader>
        <CardContent>
          <p className="mb-3 text-sm text-foreground/80">{bhyve.reason}</p>
          <Facts>
            <Fact label="vmm loaded">{yesNo(bhyve.vmm_loaded)}</Fact>
            <Fact label="vmm loadable">{yesNo(bhyve.vmm_loadable)}</Fact>
            <Fact label="Device" mono>
              {bhyve.vmm_device || "none"}
            </Fact>
            <Fact label="Binary" mono>
              {bhyve.binary_path || "not found"}
            </Fact>
          </Facts>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <div>
            <CardTitle>QEMU</CardTitle>
            <CardDescription>Optional secondary engine</CardDescription>
          </div>
          <CapabilityBadge capability={qemu} />
        </CardHeader>
        <CardContent>
          <p className="mb-3 text-sm text-foreground/80">{qemu.reason}</p>
          <Facts>
            <Fact label="Acceleration">{qemu.acceleration || "not reported"}</Fact>
            <Fact label="Binaries">
              {binaries.length === 0 ? (
                "none found"
              ) : (
                <ul className="space-y-1">
                  {binaries.map((b) => (
                    <li key={b.path}>
                      <span className="font-mono text-[0.8125rem]">{b.path}</span>
                      <span className="ml-2 text-xs text-muted-foreground">{b.version || b.error || ""}</span>
                    </li>
                  ))}
                </ul>
              )}
            </Fact>
          </Facts>
        </CardContent>
      </Card>

      <Card className="lg:col-span-2">
        <CardHeader>
          <div>
            <CardTitle>OpenZFS</CardTitle>
            <CardDescription>
              {zfs.module_loaded ? `Module loaded${zfs.module_version ? `, ${zfs.module_version}` : ""}` : "Module not loaded"}
            </CardDescription>
          </div>
          <CapabilityBadge capability={zfs} />
        </CardHeader>
        <CardContent>
          <p className="mb-3 text-sm text-foreground/80">{zfs.reason}</p>
          {zfs.pools_error ? <p className="mb-3 text-sm text-bad">{zfs.pools_error}</p> : null}
          {pools.length === 0 ? (
            <p className="text-sm text-muted-foreground">No pools are imported on this node.</p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Pool</TableHead>
                  <TableHead>Health</TableHead>
                  <TableHead className="w-1/4">Used</TableHead>
                  <TableHead className="text-right">Allocated</TableHead>
                  <TableHead className="text-right">Free</TableHead>
                  <TableHead className="text-right">Size</TableHead>
                  <TableHead className="text-right">Fragmentation</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {pools.map((p) => {
                  const pct = p.capacity_percent ?? percent(p.allocated_bytes, p.size_bytes);
                  return (
                    <TableRow key={p.name}>
                      <TableCell className="font-medium">{p.name}</TableCell>
                      <TableCell>
                        <PoolHealthBadge health={p.health} />
                      </TableCell>
                      <TableCell>
                        <div className="flex items-center gap-2">
                          <Meter value={pct} label={`Pool ${p.name} capacity used`} />
                          <span className="w-9 text-right text-xs tabular-nums text-muted-foreground">
                            {pct === null ? "" : `${Math.round(pct)}%`}
                          </span>
                        </div>
                      </TableCell>
                      <TableCell className="text-right tabular-nums">{formatBytes(p.allocated_bytes)}</TableCell>
                      <TableCell className="text-right tabular-nums">{formatBytes(p.free_bytes)}</TableCell>
                      <TableCell className="text-right tabular-nums">{formatBytes(p.size_bytes)}</TableCell>
                      <TableCell className="text-right tabular-nums">
                        {p.fragmentation_percent === null ? "n/a" : `${p.fragmentation_percent}%`}
                      </TableCell>
                    </TableRow>
                  );
                })}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>
    </div>
  );
}

export function NodesPage() {
  // Only the local node exists in this release; its ID comes from /system.
  const system = useQuery("system", (signal) => api.system(signal));
  const nodeId = system.data?.node_id;
  const hardware = useQuery(
    nodeId ? `hardware:${nodeId}` : null,
    (signal) => api.hardware(nodeId ?? "", signal),
    { refreshMs: 30_000 },
  );

  const hw = hardware.data;
  const error = system.error ?? hardware.error;
  const reload = () => {
    system.reload();
    hardware.reload();
  };

  return (
    <>
      <PageHeader
        title="Nodes"
        description={
          hw ? (
            <>
              <span className="font-medium text-foreground">{hw.host.hostname}</span> · {hw.host.os.name}{" "}
              {hw.host.os.release} · discovered {formatDateTime(hw.collected_at)}
            </>
          ) : (
            "Hardware and capabilities of the local node, discovered read-only."
          )
        }
        actions={<RefreshButton onClick={reload} updatedAt={hardware.updatedAt} />}
      />
      {error ? <ErrorNotice error={error} onRetry={reload} stale={hw !== undefined} /> : null}
      {!hw && !error ? <LoadingCards count={4} /> : null}
      {hw ? <HardwareView hw={hw} /> : null}
    </>
  );
}
