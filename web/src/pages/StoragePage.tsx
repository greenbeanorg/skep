import { useEffect, useState } from "react";
import { Link } from "react-router";

import { api, toApiError, type ApiError } from "@/api/client";
import { useQuery } from "@/api/hooks";
import { PERMISSIONS, type Dataset, type DatasetOwner, type PoolMetrics, type StoragePool, type VMMetrics } from "@/api/types";
import { useSession } from "@/app/session";
import { InlineError } from "@/components/errors";
import { ErrorNotice, Fact, Facts, LoadingCards, PageHeader, RefreshButton } from "@/components/page";
import { PoolHealthBadge, VMStateBadge } from "@/components/status";
import { Badge, type Tone } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Select } from "@/components/ui/input";
import { Notice } from "@/components/ui/notice";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { formatAgo, formatBytes, formatDuration } from "@/lib/format";
import { cn } from "@/lib/utils";

const CAPACITY_TONE: Record<PoolMetrics["capacity"], Tone> = {
  ok: "ok",
  warning: "warn",
  failing: "bad",
  unavailable: "bad",
};

const CAPACITY_BAR: Record<PoolMetrics["capacity"], string> = {
  ok: "bg-primary",
  warning: "bg-warn",
  failing: "bg-bad",
  unavailable: "bg-muted-foreground",
};

/** A pool's capacity bar, coloured by the storage_capacity thresholds. */
function CapacityMeter({ pool }: { pool: PoolMetrics }) {
  return (
    <div
      className="h-2 w-full overflow-hidden rounded-full bg-muted"
      role="meter"
      aria-label={`${pool.name} capacity`}
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={Math.round(pool.capacity_percent)}
    >
      <div className={cn("h-full rounded-full", CAPACITY_BAR[pool.capacity])} style={{ width: `${pool.capacity_percent}%` }} />
    </div>
  );
}

/** The pool cards, from the metrics when the role may read them. */
function PoolCards({ metrics, pools }: { metrics?: PoolMetrics[]; pools: StoragePool[] }) {
  if (pools.length === 0) {
    return (
      <Notice tone="info" title="No storage is allow-listed">
        An administrator lists ZFS pools in <code>storage.pools</code> and directory pools in <code>storage.directories</code> of
        the agent's configuration.
      </Notice>
    );
  }
  return (
    <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
      {pools.map((p) => {
        const m = metrics?.find((x) => x.name === p.name);
        return (
          <Card key={p.name} data-testid={`pool-${p.name}`}>
            <CardHeader>
              <div>
                <CardTitle>{p.name}</CardTitle>
                <CardDescription>{p.type === "zfs" ? "ZFS pool" : "Directory pool"}</CardDescription>
              </div>
              <PoolHealthBadge health={p.health} />
            </CardHeader>
            <CardContent className="space-y-3">
              {p.error ? <Notice tone="bad">{p.error}</Notice> : null}
              {m && m.capacity !== "unavailable" ? (
                <div className="space-y-1">
                  <div className="flex items-center justify-between text-sm">
                    <span>{m.capacity_percent.toFixed(1)}% in use</span>
                    {m.capacity !== "ok" ? <Badge tone={CAPACITY_TONE[m.capacity]}>{m.capacity}</Badge> : null}
                  </div>
                  <CapacityMeter pool={m} />
                </div>
              ) : null}
              <Facts>
                <Fact label="Size">{formatBytes(p.size_bytes)}</Fact>
                <Fact label="Allocated">{formatBytes(p.allocated_bytes)}</Fact>
                <Fact label="Available">{formatBytes(p.available_bytes)}</Fact>
                <Fact label="Snapshots, clones">{p.capabilities.snapshots ? "yes" : "no (directory pool)"}</Fact>
              </Facts>
              {m?.locked_datasets?.length ? (
                <Notice tone="bad" title="Encryption key not loaded">
                  {m.locked_datasets.join(", ")}. An administrator loads the key on the host; Skep holds none.
                </Notice>
              ) : null}
            </CardContent>
          </Card>
        );
      })}
    </div>
  );
}

function VMUsage({ vms }: { vms: VMMetrics[] }) {
  if (vms.length === 0) return <p className="px-2 text-sm text-muted-foreground">No virtual machines.</p>;
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>VM</TableHead>
          <TableHead>State</TableHead>
          <TableHead>CPU time</TableHead>
          <TableHead>Resident memory</TableHead>
          <TableHead>Disk space held</TableHead>
          <TableHead>Disks reference</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {vms.map((v) => (
          <TableRow key={v.id}>
            <TableCell>
              <Link to={`/vms/${v.id}`} className="font-medium hover:underline">
                {v.name}
              </Link>
              {v.error ? <div className="text-xs text-bad">{v.error}</div> : null}
            </TableCell>
            <TableCell>
              <VMStateBadge state={v.state} />
            </TableCell>
            <TableCell>{v.cpu_time_seconds !== undefined ? formatDuration(v.cpu_time_seconds) : "—"}</TableCell>
            <TableCell>{v.resident_memory_bytes !== undefined ? formatBytes(v.resident_memory_bytes) : "—"}</TableCell>
            <TableCell>{formatBytes(v.disk_used_bytes)}</TableCell>
            <TableCell>{formatBytes(v.disk_referenced_bytes)}</TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

const OWNER_TONE: Record<DatasetOwner["kind"], Tone> = {
  vm: "ok",
  container: "ok",
  template: "info",
  skep: "neutral",
  unowned: "warn",
};

function OwnerCell({ owner }: { owner: DatasetOwner }) {
  return (
    <div className="space-y-0.5">
      <Badge tone={OWNER_TONE[owner.kind]}>{owner.kind}</Badge>
      {owner.kind === "vm" && owner.id ? (
        <div>
          <Link to={`/vms/${owner.id}`} className="font-mono text-xs hover:underline">
            {owner.id.slice(0, 8)}
          </Link>
        </div>
      ) : null}
      {owner.reason ? <div className="text-xs text-muted-foreground">{owner.reason}</div> : null}
    </div>
  );
}

/** The inventory of what Skep owns in its ZFS pools (ADR 0010 decision 1). */
function Inventory({ pools }: { pools: string[] }) {
  const [pool, setPool] = useState("");
  const [items, setItems] = useState<Dataset[]>([]);
  const [cursor, setCursor] = useState<string | undefined>();
  const [unavailable, setUnavailable] = useState<string[]>([]);
  const [error, setError] = useState<ApiError | undefined>();
  const [loading, setLoading] = useState(true);
  const [generation, setGeneration] = useState(0);

  const load = async (next?: string) => {
    setLoading(true);
    setError(undefined);
    try {
      const page = await api.datasets({ pool: pool || undefined, cursor: next });
      setItems((old) => (next ? [...old, ...page.items] : page.items));
      setCursor(page.next_cursor);
      setUnavailable(page.unavailable_pools ?? []);
    } catch (err) {
      setError(toApiError(err));
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    void load();
  }, [pool, generation]);

  return (
    <Card>
      <CardHeader className="flex flex-row flex-wrap items-center justify-between gap-2">
        <div>
          <CardTitle>Datasets</CardTitle>
          <CardDescription>
            Everything under each ZFS pool's Skep root. Unowned entries are listed and never removed automatically.
          </CardDescription>
        </div>
        <div className="flex items-center gap-2">
          <Select aria-label="Pool" value={pool} onChange={(e) => setPool(e.target.value)} className="w-40">
            <option value="">All ZFS pools</option>
            {pools.map((p) => (
              <option key={p} value={p}>
                {p}
              </option>
            ))}
          </Select>
          <Button size="sm" variant="outline" onClick={() => setGeneration((g) => g + 1)}>
            Reload
          </Button>
        </div>
      </CardHeader>
      <CardContent className="space-y-3 px-2">
        {unavailable.length ? <Notice tone="warn">Could not be read: {unavailable.join(", ")}.</Notice> : null}
        {error ? <InlineError error={error} /> : null}
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Type</TableHead>
              <TableHead>Owner</TableHead>
              <TableHead>Used</TableHead>
              <TableHead>Referenced</TableHead>
              <TableHead>Size</TableHead>
              <TableHead>Compression</TableHead>
              <TableHead>Encryption</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {items.map((d) => (
              <TableRow key={d.name}>
                <TableCell className="font-mono text-xs break-all">
                  {d.name}
                  {d.origin ? <div className="font-sans text-muted-foreground">clone of {d.origin}</div> : null}
                </TableCell>
                <TableCell className="text-xs">{d.type}</TableCell>
                <TableCell>
                  <OwnerCell owner={d.owner} />
                </TableCell>
                <TableCell className="text-xs">{formatBytes(d.used_bytes)}</TableCell>
                <TableCell className="text-xs">{formatBytes(d.referenced_bytes)}</TableCell>
                <TableCell className="text-xs">
                  {d.volsize_bytes ? formatBytes(d.volsize_bytes) : "—"}
                  {d.type === "volume" && !d.refreservation_bytes ? <span className="text-muted-foreground"> thin</span> : null}
                </TableCell>
                <TableCell className="text-xs">
                  {d.compression ?? "—"}
                  {d.compressratio ? <span className="text-muted-foreground"> {d.compressratio}</span> : null}
                </TableCell>
                <TableCell className="text-xs">
                  {d.encryption && d.encryption !== "off" ? (
                    <Badge tone={d.keystatus === "unavailable" ? "bad" : "ok"}>
                      {d.encryption}, key {d.keystatus === "unavailable" ? "not loaded" : "loaded"}
                    </Badge>
                  ) : (
                    "off"
                  )}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
        {!loading && items.length === 0 && !error ? <p className="px-2 text-sm text-muted-foreground">Nothing yet.</p> : null}
        {cursor ? (
          <div className="px-2">
            <Button size="sm" variant="outline" disabled={loading} onClick={() => void load(cursor)}>
              Load more
            </Button>
          </div>
        ) : null}
      </CardContent>
    </Card>
  );
}

/**
 * The Storage section (ADR 0010 decisions 1, 6, and 15): the allow-listed
 * pools with their capacity against the health thresholds, the current
 * disk and process use of each VM, and the inventory of the ZFS datasets
 * Skep owns. Current values only; there is no history.
 */
export function StoragePage() {
  const { can } = useSession();
  const canMetrics = can(PERMISSIONS.systemRead) && can(PERMISSIONS.vmRead) && can(PERMISSIONS.storageRead);
  const pools = useQuery("storage:pools", (o) => api.storagePools(o), { refreshMs: 30_000 });
  const metrics = useQuery(canMetrics ? "metrics" : null, (o) => api.metrics(o), { refreshMs: 30_000 });
  const reload = () => {
    pools.reload();
    metrics.reload();
  };

  if (pools.loading) return <LoadingCards count={2} />;
  const poolList = pools.data?.items ?? [];
  const m = metrics.data;
  return (
    <>
      <PageHeader
        title="Storage"
        description={
          m ? (
            <span>
              Capacity warns at {m.capacity_warn_percent}% and fails at {m.capacity_fail_percent}%. Sampled{" "}
              {formatAgo(m.observed_at)}.
            </span>
          ) : (
            "The pools Skep may use."
          )
        }
        actions={<RefreshButton onClick={reload} updatedAt={pools.updatedAt} />}
      />
      {pools.error ? <ErrorNotice error={pools.error} onRetry={pools.reload} stale={!!pools.data} /> : null}
      {metrics.error ? <ErrorNotice error={metrics.error} onRetry={metrics.reload} stale={!!metrics.data} /> : null}
      <div className="space-y-4">
        <PoolCards metrics={m?.pools} pools={poolList} />
        {m ? (
          <Card>
            <CardHeader>
              <div>
                <CardTitle>Virtual machines</CardTitle>
                <CardDescription>Disk space in their pools, snapshots included; CPU time and memory of bhyve while running.</CardDescription>
              </div>
            </CardHeader>
            <CardContent className="px-2">
              <VMUsage vms={m.vms} />
            </CardContent>
          </Card>
        ) : null}
        <Inventory pools={poolList.filter((p) => p.type === "zfs").map((p) => p.name)} />
      </div>
    </>
  );
}
