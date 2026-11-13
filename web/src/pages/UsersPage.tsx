import { useState } from "react";
import { NavLink, Route, Routes } from "react-router";

import { api } from "@/api/client";
import { useQuery } from "@/api/hooks";
import { PERMISSIONS, type Role, type User } from "@/api/types";
import { useIdentity, useSession } from "@/app/session";
import { ErrorNotice } from "@/components/errors";
import { PageHeader, RefreshButton } from "@/components/page";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { formatDateTime } from "@/lib/format";
import { cn } from "@/lib/utils";
import { AuditPage } from "@/pages/users/AuditPage";
import { RolesPage } from "@/pages/users/RolesPage";
import { CreateUserDialog, DeleteUserDialog, EditUserDialog, SetPasswordDialog, ToggleUserDialog } from "@/pages/users/dialogs";

import { ForbiddenPage } from "./ForbiddenPage";

function Tabs() {
  const { can } = useSession();
  const tabs = [
    { to: "/users", label: "Users", end: true },
    ...(can(PERMISSIONS.roleRead) ? [{ to: "/users/roles", label: "Roles", end: false }] : []),
    ...(can(PERMISSIONS.auditRead) ? [{ to: "/users/audit", label: "Access audit log", end: false }] : []),
  ];
  return (
    <nav aria-label="Users and permissions" className="mb-5 flex gap-1 border-b">
      {tabs.map((t) => (
        <NavLink
          key={t.to}
          to={t.to}
          end={t.end}
          className={({ isActive }) =>
            cn(
              "-mb-px border-b-2 px-3 py-2 text-sm transition-colors",
              isActive ? "border-primary font-medium text-foreground" : "border-transparent text-muted-foreground hover:text-foreground",
            )
          }
        >
          {t.label}
        </NavLink>
      ))}
    </nav>
  );
}

type Action =
  | { kind: "create" }
  | { kind: "edit"; user: User }
  | { kind: "password"; user: User }
  | { kind: "toggle"; user: User }
  | { kind: "delete"; user: User };

function UsersList() {
  const { can } = useSession();
  const me = useIdentity().user;
  const manage = can(PERMISSIONS.userManage);
  const users = useQuery("users", (o) => api.users(o), { refreshMs: 30_000 });
  const roles = useQuery(can(PERMISSIONS.roleRead) ? "roles" : null, (o) => api.roles(o));
  const [action, setAction] = useState<Action | undefined>();
  const close = () => setAction(undefined);
  const done = () => {
    close();
    users.reload();
  };
  const items = users.data?.items ?? [];
  const roleList: Role[] = roles.data?.items ?? [];

  return (
    <>
      <div className="mb-4 flex flex-wrap items-center justify-between gap-3">
        <p className="text-sm text-muted-foreground">
          {manage
            ? "Every change here is written to the access audit log. Disabling, deleting, a role change, and a new password each end that user's sessions."
            : "Your role can read accounts but not change them."}
        </p>
        <div className="flex items-center gap-2">
          <RefreshButton onClick={users.reload} updatedAt={users.updatedAt} />
          {manage ? (
            <Button size="sm" onClick={() => setAction({ kind: "create" })}>
              New user
            </Button>
          ) : null}
        </div>
      </div>

      {users.error ? <ErrorNotice error={users.error} onRetry={users.reload} stale={users.data !== undefined} /> : null}

      <Card>
        <CardContent className="px-2 pt-2 pb-2">
          {users.loading ? (
            <div className="space-y-2 p-3" aria-busy="true">
              <Skeleton className="h-5 w-full" />
              <Skeleton className="h-5 w-2/3" />
            </div>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>User</TableHead>
                  <TableHead>Role</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Last login</TableHead>
                  <TableHead>Password changed</TableHead>
                  {manage ? <TableHead className="text-right">Actions</TableHead> : null}
                </TableRow>
              </TableHeader>
              <TableBody>
                {items.map((u) => (
                  <TableRow key={u.id}>
                    <TableCell>
                      <div className="font-medium">{u.display_name || u.username}</div>
                      <div className="font-mono text-xs text-muted-foreground">
                        {u.username}
                        {u.id === me.id ? <span className="ml-2 font-sans">(you)</span> : null}
                      </div>
                    </TableCell>
                    <TableCell>{u.role}</TableCell>
                    <TableCell>{u.disabled ? <Badge tone="warn">disabled</Badge> : <Badge tone="ok">enabled</Badge>}</TableCell>
                    <TableCell className="text-xs whitespace-nowrap">{u.last_login_at ? formatDateTime(u.last_login_at) : "never"}</TableCell>
                    <TableCell className="text-xs whitespace-nowrap">
                      {u.password_changed_at ? formatDateTime(u.password_changed_at) : "never"}
                    </TableCell>
                    {manage ? (
                      <TableCell className="text-right">
                        <div className="flex flex-wrap justify-end gap-1">
                          <Button variant="ghost" size="sm" onClick={() => setAction({ kind: "edit", user: u })}>
                            Edit
                          </Button>
                          <Button variant="ghost" size="sm" onClick={() => setAction({ kind: "password", user: u })}>
                            Set password
                          </Button>
                          <Button variant="ghost" size="sm" onClick={() => setAction({ kind: "toggle", user: u })}>
                            {u.disabled ? "Enable" : "Disable"}
                          </Button>
                          <Button
                            variant="ghost"
                            size="sm"
                            className="text-bad hover:text-bad"
                            onClick={() => setAction({ kind: "delete", user: u })}
                          >
                            Delete
                          </Button>
                        </div>
                      </TableCell>
                    ) : null}
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      {action?.kind === "create" ? <CreateUserDialog roles={roleList} onClose={close} onDone={done} /> : null}
      {action?.kind === "edit" ? <EditUserDialog user={action.user} roles={roleList} onClose={close} onDone={done} /> : null}
      {action?.kind === "password" ? <SetPasswordDialog user={action.user} onClose={close} onDone={done} /> : null}
      {action?.kind === "toggle" ? <ToggleUserDialog user={action.user} onClose={close} onDone={done} /> : null}
      {action?.kind === "delete" ? <DeleteUserDialog user={action.user} onClose={close} onDone={done} /> : null}
    </>
  );
}

/** Users and permissions: accounts, roles, and the access audit log. */
export function UsersPage() {
  const { can } = useSession();
  return (
    <>
      <PageHeader title="Users and permissions" description="Accounts, their roles, and the access audit log of this node." />
      <Tabs />
      <Routes>
        <Route index element={<UsersList />} />
        <Route
          path="roles"
          element={can(PERMISSIONS.roleRead) ? <RolesPage /> : <ForbiddenPage permission={PERMISSIONS.roleRead} />}
        />
        <Route
          path="audit"
          element={can(PERMISSIONS.auditRead) ? <AuditPage /> : <ForbiddenPage permission={PERMISSIONS.auditRead} />}
        />
      </Routes>
    </>
  );
}
