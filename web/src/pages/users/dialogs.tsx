import { useState, type FormEvent, type ReactNode } from "react";

import { api, toApiError, type ApiError } from "@/api/client";
import type { Role, User } from "@/api/types";
import { useIdentity } from "@/app/session";
import { InlineError } from "@/components/errors";
import { Button } from "@/components/ui/button";
import { Dialog, DialogActions } from "@/components/ui/dialog";
import { FieldText, Input, Label, Select } from "@/components/ui/input";
import { Notice } from "@/components/ui/notice";
import { MAX_PASSWORD_LENGTH, MIN_PASSWORD_LENGTH, passwordProblem } from "@/pages/AccountPage";

/** The username rule of the API (NewUser.username in api/openapi.yaml). */
export const USERNAME_RE = /^[a-z][a-z0-9._-]{0,31}$/;

interface DialogBase {
  onClose: () => void;
  /** Called after a successful change; the list reloads. */
  onDone: () => void;
}

/** The message for a user-management refusal (ADR 0008, decision 7). */
export function userErrorMessage(err: ApiError): string | undefined {
  switch (err.code) {
    case "last_admin":
      return "This is the only enabled administrator. It cannot be disabled, deleted, or given another role.";
    case "username_taken":
      return "A user with this username already exists.";
    case "unknown_role":
      return "No such role.";
  }
  return undefined;
}

function withMessage(err: ApiError): ApiError {
  const text = userErrorMessage(err);
  return text ? Object.assign(err, { message: text }) : err;
}

/** Runs one write; keeps the form's values (except passwords) on failure. */
function useSubmit(onDone: () => void) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<ApiError | undefined>();
  const run = async (fn: () => Promise<unknown>) => {
    if (busy) return;
    setBusy(true);
    setError(undefined);
    try {
      await fn();
      onDone();
    } catch (err) {
      setError(withMessage(toApiError(err)));
    } finally {
      setBusy(false);
    }
  };
  return { busy, error, run };
}

function RoleSelect({ id, value, onChange, roles, disabled }: { id: string; value: string; onChange: (v: string) => void; roles: Role[]; disabled?: boolean }) {
  const names = roles.length > 0 ? roles.map((r) => r.name) : ["admin", "operator", "viewer"];
  return (
    <Select id={id} value={value} onChange={(e) => onChange(e.target.value)} disabled={disabled}>
      {names.map((n) => (
        <option key={n} value={n}>
          {n}
          {roles.find((r) => r.name === n)?.description ? ` – ${roles.find((r) => r.name === n)?.description}` : ""}
        </option>
      ))}
    </Select>
  );
}

function PasswordFields({
  next,
  confirm,
  setNext,
  setConfirm,
  disabled,
  label,
}: {
  next: string;
  confirm: string;
  setNext: (v: string) => void;
  setConfirm: (v: string) => void;
  disabled: boolean;
  label: string;
}) {
  const problem = next === "" && confirm === "" ? undefined : passwordProblem(next, confirm);
  return (
    <>
      <div className="space-y-1.5">
        <Label htmlFor="user-password">{label}</Label>
        <Input
          id="user-password"
          type="password"
          autoComplete="new-password"
          required
          minLength={MIN_PASSWORD_LENGTH}
          maxLength={MAX_PASSWORD_LENGTH}
          value={next}
          onChange={(e) => setNext(e.target.value)}
          disabled={disabled}
        />
        <FieldText>
          {MIN_PASSWORD_LENGTH} to {MAX_PASSWORD_LENGTH} characters, no composition rules.
        </FieldText>
      </div>
      <div className="space-y-1.5">
        <Label htmlFor="user-password-confirm">{label}, again</Label>
        <Input
          id="user-password-confirm"
          type="password"
          autoComplete="new-password"
          required
          value={confirm}
          onChange={(e) => setConfirm(e.target.value)}
          disabled={disabled}
        />
        {problem ? <FieldText error>{problem}</FieldText> : null}
      </div>
    </>
  );
}

export function CreateUserDialog({ roles, onClose, onDone }: DialogBase & { roles: Role[] }) {
  const [username, setUsername] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [role, setRole] = useState("viewer");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const { busy, error, run } = useSubmit(onDone);
  const nameOK = USERNAME_RE.test(username);
  const pwProblem = passwordProblem(password, confirm);

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (!nameOK || pwProblem) return;
    void run(() =>
      api.createUser({ username, display_name: displayName || undefined, password, role }).catch((err) => {
        setPassword("");
        setConfirm("");
        throw err;
      }),
    );
  };

  return (
    <Dialog open onClose={onClose} title="New user" description="The account can log in at once with this password.">
      <form onSubmit={submit} className="space-y-4" noValidate>
        <div className="space-y-1.5">
          <Label htmlFor="user-username">Username</Label>
          <Input
            id="user-username"
            autoComplete="off"
            autoCapitalize="none"
            spellCheck={false}
            required
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            disabled={busy}
            aria-invalid={username !== "" && !nameOK}
          />
          <FieldText error={username !== "" && !nameOK}>
            A lowercase letter, then up to 31 lowercase letters, digits, dots, underscores, or hyphens.
          </FieldText>
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="user-display">Display name (optional)</Label>
          <Input id="user-display" maxLength={128} value={displayName} onChange={(e) => setDisplayName(e.target.value)} disabled={busy} />
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="user-role">Role</Label>
          <RoleSelect id="user-role" value={role} onChange={setRole} roles={roles} disabled={busy} />
        </div>
        <PasswordFields next={password} confirm={confirm} setNext={setPassword} setConfirm={setConfirm} disabled={busy} label="Password" />
        {error ? <InlineError error={error} /> : null}
        <DialogActions>
          <Button type="button" variant="outline" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button type="submit" disabled={busy || !nameOK || pwProblem !== undefined}>
            {busy ? "Creating…" : "Create user"}
          </Button>
        </DialogActions>
      </form>
    </Dialog>
  );
}

export function EditUserDialog({ user, roles, onClose, onDone }: DialogBase & { user: User; roles: Role[] }) {
  const me = useIdentity().user;
  const [displayName, setDisplayName] = useState(user.display_name);
  const [role, setRole] = useState(user.role);
  const { busy, error, run } = useSubmit(onDone);
  const roleChanged = role !== user.role;
  const changed = roleChanged || displayName !== user.display_name;

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (!changed) return;
    void run(() =>
      api.updateUser(user.id, {
        ...(displayName !== user.display_name ? { display_name: displayName } : {}),
        ...(roleChanged ? { role } : {}),
      }),
    );
  };

  return (
    <Dialog open onClose={onClose} title={`Edit ${user.username}`}>
      <form onSubmit={submit} className="space-y-4" noValidate>
        <div className="space-y-1.5">
          <Label htmlFor="edit-display">Display name</Label>
          <Input id="edit-display" maxLength={128} value={displayName} onChange={(e) => setDisplayName(e.target.value)} disabled={busy} />
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="edit-role">Role</Label>
          <RoleSelect id="edit-role" value={role} onChange={setRole} roles={roles} disabled={busy} />
        </div>
        {roleChanged ? (
          <Notice tone="warn" title="A role change ends every session of this user">
            {user.id === me.id ? "That includes your own: you will be returned to the login page." : "They have to log in again."}
          </Notice>
        ) : null}
        {error ? <InlineError error={error} /> : null}
        <DialogActions>
          <Button type="button" variant="outline" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button type="submit" disabled={busy || !changed}>
            {busy ? "Saving…" : "Save"}
          </Button>
        </DialogActions>
      </form>
    </Dialog>
  );
}

export function SetPasswordDialog({ user, onClose, onDone }: DialogBase & { user: User }) {
  const me = useIdentity().user;
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const { busy, error, run } = useSubmit(onDone);
  const problem = passwordProblem(password, confirm);

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (problem) return;
    void run(() =>
      api.updateUser(user.id, { password }).catch((err) => {
        setPassword("");
        setConfirm("");
        throw err;
      }),
    );
  };

  return (
    <Dialog open onClose={onClose} title={`Set a password for ${user.username}`}>
      <form onSubmit={submit} className="space-y-4" noValidate>
        <PasswordFields next={password} confirm={confirm} setNext={setPassword} setConfirm={setConfirm} disabled={busy} label="New password" />
        <Notice tone="warn" title="A new password ends every session of this user">
          {user.id === me.id
            ? "That includes your own: you will be returned to the login page. To change your own password with the current one, use Your account."
            : "They have to log in again with the new password."}
        </Notice>
        {error ? <InlineError error={error} /> : null}
        <DialogActions>
          <Button type="button" variant="outline" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button type="submit" disabled={busy || problem !== undefined}>
            {busy ? "Setting…" : "Set password"}
          </Button>
        </DialogActions>
      </form>
    </Dialog>
  );
}

export function ToggleUserDialog({ user, onClose, onDone }: DialogBase & { user: User }) {
  const me = useIdentity().user;
  const { busy, error, run } = useSubmit(onDone);
  const disabling = !user.disabled;
  return (
    <Dialog open onClose={onClose} title={disabling ? `Disable ${user.username}?` : `Enable ${user.username}?`}>
      {disabling ? (
        <Notice tone="warn" title="Disabling ends every session of this user">
          {user.id === me.id
            ? "This is your own account. If it is the only enabled administrator the server refuses; otherwise you are logged out."
            : "They cannot log in until the account is enabled again. Nothing else is removed."}
        </Notice>
      ) : (
        <p className="text-sm text-muted-foreground">The user can log in again with their password.</p>
      )}
      {error ? (
        <div className="mt-4">
          <InlineError error={error} />
        </div>
      ) : null}
      <DialogActions>
        <Button type="button" variant="outline" onClick={onClose} disabled={busy}>
          Cancel
        </Button>
        <Button
          type="button"
          variant={disabling ? "destructive" : "default"}
          disabled={busy}
          onClick={() => void run(() => api.updateUser(user.id, { disabled: disabling }))}
        >
          {busy ? "Saving…" : disabling ? "Disable" : "Enable"}
        </Button>
      </DialogActions>
    </Dialog>
  );
}

export function DeleteUserDialog({ user, onClose, onDone }: DialogBase & { user: User }) {
  const me = useIdentity().user;
  const [typed, setTyped] = useState("");
  const { busy, error, run } = useSubmit(onDone);
  const confirmed = typed === user.username;
  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (!confirmed) return;
    void run(() => api.deleteUser(user.id));
  };
  return (
    <Dialog open onClose={onClose} title={`Delete ${user.username}?`} description="This cannot be undone. The audit log keeps the user's past actions.">
      <form onSubmit={submit} className="space-y-4" noValidate>
        <Notice tone="bad" title="Deleting ends every session of this user">
          {user.id === me.id ? "This is your own account." : null}
        </Notice>
        <div className="space-y-1.5">
          <Label htmlFor="delete-confirm">
            Type the username <span className="font-mono">{user.username}</span> to confirm
          </Label>
          <Input id="delete-confirm" autoComplete="off" spellCheck={false} value={typed} onChange={(e) => setTyped(e.target.value)} disabled={busy} />
        </div>
        {error ? <InlineError error={error} /> : null}
        <DialogActions>
          <Button type="button" variant="outline" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button type="submit" variant="destructive" disabled={busy || !confirmed}>
            {busy ? "Deleting…" : "Delete user"}
          </Button>
        </DialogActions>
      </form>
    </Dialog>
  );
}

/** A generic confirmation, used for task cancel. */
export function ConfirmDialog({
  title,
  description,
  children,
  confirmLabel,
  destructive,
  onConfirm,
  onClose,
  error,
  busy,
}: {
  title: ReactNode;
  description?: ReactNode;
  children?: ReactNode;
  confirmLabel: string;
  destructive?: boolean;
  onConfirm: () => void;
  onClose: () => void;
  error?: ApiError;
  busy?: boolean;
}) {
  return (
    <Dialog open onClose={onClose} title={title} description={description}>
      {children}
      {error ? (
        <div className="mt-4">
          <InlineError error={error} />
        </div>
      ) : null}
      <DialogActions>
        <Button type="button" variant="outline" onClick={onClose} disabled={busy}>
          Cancel
        </Button>
        <Button type="button" variant={destructive ? "destructive" : "default"} onClick={onConfirm} disabled={busy}>
          {busy ? "Working…" : confirmLabel}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
