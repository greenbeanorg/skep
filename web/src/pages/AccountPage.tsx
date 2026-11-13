import { useState, type FormEvent } from "react";

import { api, toApiError, type ApiError } from "@/api/client";
import { useIdentity, useSession } from "@/app/session";
import { InlineError } from "@/components/errors";
import { Fact, Facts, PageHeader } from "@/components/page";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { FieldText, Input, Label } from "@/components/ui/input";
import { Notice } from "@/components/ui/notice";
import { formatDateTime, formatDuration } from "@/lib/format";

export const MIN_PASSWORD_LENGTH = 12;
export const MAX_PASSWORD_LENGTH = 256;

/** Checks a new password the way the API does: length only, no composition rules. */
export function passwordProblem(password: string, confirmation: string): string | undefined {
  if (password.length < MIN_PASSWORD_LENGTH) return `The new password needs at least ${MIN_PASSWORD_LENGTH} characters.`;
  if (password.length > MAX_PASSWORD_LENGTH) return `The new password may have at most ${MAX_PASSWORD_LENGTH} characters.`;
  if (password !== confirmation) return "The two entries of the new password differ.";
  return undefined;
}

function ChangePasswordForm() {
  const { passwordChanged } = useSession();
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [confirm, setConfirm] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<ApiError | undefined>();
  const problem = next === "" && confirm === "" ? undefined : passwordProblem(next, confirm);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (busy || problem) return;
    setBusy(true);
    setError(undefined);
    try {
      await api.changePassword({ current_password: current, new_password: next });
      // Every session of the user ended, this one included: back to the
      // login page, which says why (ADR 0008, decision 7).
      passwordChanged();
    } catch (err) {
      setError(toApiError(err));
      setCurrent("");
    } finally {
      setBusy(false);
    }
  };

  return (
    <form onSubmit={submit} className="space-y-4" aria-label="Change password" noValidate>
      <div className="space-y-1.5">
        <Label htmlFor="pw-current">Current password</Label>
        <Input
          id="pw-current"
          type="password"
          autoComplete="current-password"
          required
          value={current}
          onChange={(e) => setCurrent(e.target.value)}
          disabled={busy}
        />
      </div>
      <div className="space-y-1.5">
        <Label htmlFor="pw-new">New password</Label>
        <Input
          id="pw-new"
          type="password"
          autoComplete="new-password"
          required
          minLength={MIN_PASSWORD_LENGTH}
          maxLength={MAX_PASSWORD_LENGTH}
          value={next}
          onChange={(e) => setNext(e.target.value)}
          disabled={busy}
          aria-invalid={problem !== undefined && next !== ""}
        />
        <FieldText>
          {MIN_PASSWORD_LENGTH} to {MAX_PASSWORD_LENGTH} characters. There are no composition rules; a long passphrase
          is a good choice.
        </FieldText>
      </div>
      <div className="space-y-1.5">
        <Label htmlFor="pw-confirm">New password, again</Label>
        <Input
          id="pw-confirm"
          type="password"
          autoComplete="new-password"
          required
          value={confirm}
          onChange={(e) => setConfirm(e.target.value)}
          disabled={busy}
        />
        {problem ? <FieldText error>{problem}</FieldText> : null}
      </div>
      {error ? (
        <InlineError
          error={
            error.code === "invalid_credentials"
              ? Object.assign(error, { message: "The current password is not correct." })
              : error
          }
        />
      ) : null}
      <Notice tone="warn" title="Changing the password ends every session of your account">
        This one included: you are returned to the login page. Other browsers and skepctl sessions are logged out too.
      </Notice>
      <Button type="submit" disabled={busy || current === "" || next === "" || problem !== undefined}>
        {busy ? "Changing…" : "Change password"}
      </Button>
    </form>
  );
}

/** The own account: who you are, the session in use, the password. */
export function AccountPage() {
  const { user, session, permissions } = useIdentity();
  return (
    <>
      <PageHeader title="Your account" description="Who you are logged in as, and the session in use." />
      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <div>
              <CardTitle>Identity</CardTitle>
              <CardDescription>As the server reports it</CardDescription>
            </div>
          </CardHeader>
          <CardContent>
            <Facts>
              <Fact label="Username" mono>
                {user.username}
              </Fact>
              <Fact label="Display name">{user.display_name || <span className="text-muted-foreground">none</span>}</Fact>
              <Fact label="Role">{user.role}</Fact>
              <Fact label="Permissions">
                {permissions.includes("*") ? (
                  "everything"
                ) : (
                  <span className="font-mono text-[0.8125rem]">{permissions.join(", ") || "none"}</span>
                )}
              </Fact>
              <Fact label="Password changed">
                {user.password_changed_at ? formatDateTime(user.password_changed_at) : "never"}
              </Fact>
              <Fact label="Previous login">
                {user.last_login_at ? formatDateTime(user.last_login_at) : "none recorded"}
              </Fact>
            </Facts>
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <div>
              <CardTitle>This session</CardTitle>
              <CardDescription>Lifetimes are set by the administrator of this host</CardDescription>
            </div>
          </CardHeader>
          <CardContent>
            <Facts>
              <Fact label="Kind">{session.kind}</Fact>
              <Fact label="Started">{formatDateTime(session.created_at)}</Fact>
              <Fact label="Ends at the latest">{formatDateTime(session.expires_at)}</Fact>
              <Fact label="Idle timeout">{formatDuration(session.idle_timeout_seconds)} without activity</Fact>
            </Facts>
            <p className="mt-3 text-xs text-muted-foreground">
              Automatic refreshes of this interface do not count as activity, so an unattended page logs itself out.
            </p>
          </CardContent>
        </Card>

        <Card className="lg:col-span-2">
          <CardHeader>
            <div>
              <CardTitle>Change password</CardTitle>
              <CardDescription>Needs the current password</CardDescription>
            </div>
          </CardHeader>
          <CardContent>
            <div className="max-w-md">
              <ChangePasswordForm />
            </div>
          </CardContent>
        </Card>
      </div>
    </>
  );
}
