import { useEffect, useState, type FormEvent } from "react";

import { toApiError, type ApiError } from "@/api/client";
import { useSession, type AnonymousReason } from "@/app/session";
import { useTheme } from "@/app/theme";
import { MoonIcon, SkepMark, SunIcon } from "@/components/icons";
import { Button } from "@/components/ui/button";
import { FieldText, Input, Label } from "@/components/ui/input";
import { Notice } from "@/components/ui/notice";

const REASONS: Record<AnonymousReason, { title: string; text: string }> = {
  expired: { title: "Your session has ended", text: "Log in again to continue where you were." },
  logged_out: { title: "You are logged out", text: "" },
  logged_out_elsewhere: { title: "You were logged out in another tab", text: "" },
  password_changed: {
    title: "Your password was changed",
    text: "Every session of your account was ended, this one included. Log in with the new password.",
  },
};

/** The message for a failed login (ADR 0008, decision 5). */
export function loginErrorMessage(err: ApiError): { text: string; command?: string } {
  switch (err.code) {
    case "invalid_credentials":
      return { text: "The username or password is not correct." };
    case "rate_limited":
      return { text: `Too many attempts. Wait ${err.retryAfter ?? "a few"} seconds and try again.` };
    case "setup_required":
      return {
        text: "No administrator exists yet. Create one on the host, then log in:",
        command: "skep-api admin create --username NAME",
      };
    case "network_error":
      return { text: "skep-api cannot be reached. Check that the service is running, then try again." };
    case "forbidden":
      return {
        text:
          "The request was refused by the origin check. This should not happen from this page; " +
          `report request ${err.requestId ?? "(no ID)"} to the administrator.`,
      };
    default:
      return { text: `${err.message} (${err.code}${err.requestId ? `, request ${err.requestId}` : ""})` };
  }
}

/**
 * The login page (ADR 0008, decision 2). It is shown at whatever address
 * the user was at, so a login returns there. There is no "remember me":
 * the lifetimes are the server's.
 */
export function LoginPage({ reason }: { reason: AnonymousReason | undefined }) {
  const { login } = useSession();
  const { theme, setChoice } = useTheme();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<ApiError | undefined>();
  const [waitUntil, setWaitUntil] = useState<number | undefined>();
  const [now, setNow] = useState(Date.now());

  // While rate limited, the button stays disabled until Retry-After has
  // passed.
  useEffect(() => {
    if (waitUntil === undefined) return;
    const id = window.setInterval(() => setNow(Date.now()), 500);
    return () => window.clearInterval(id);
  }, [waitUntil]);
  const waiting = waitUntil !== undefined && now < waitUntil;
  useEffect(() => {
    if (waitUntil !== undefined && !waiting) setWaitUntil(undefined);
  }, [waitUntil, waiting]);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (busy || waiting) return;
    setBusy(true);
    setError(undefined);
    try {
      await login(username.trim(), password);
      // The provider switches to the app; this component goes away.
    } catch (err) {
      const apiErr = toApiError(err);
      setError(apiErr);
      // Passwords are never kept after a failure.
      setPassword("");
      if (apiErr.code === "rate_limited") setWaitUntil(Date.now() + (apiErr.retryAfter ?? 5) * 1000);
    } finally {
      setBusy(false);
    }
  };

  const why = reason ? REASONS[reason] : undefined;
  const failure = error ? loginErrorMessage(error) : undefined;
  const secondsLeft = waiting && waitUntil !== undefined ? Math.ceil((waitUntil - now) / 1000) : 0;

  return (
    <div className="flex min-h-dvh flex-col bg-background">
      <header className="flex h-14 items-center justify-end px-4">
        <Button
          variant="ghost"
          size="icon"
          onClick={() => setChoice(theme === "dark" ? "light" : "dark")}
          aria-label={theme === "dark" ? "Switch to the light theme" : "Switch to the dark theme"}
        >
          {theme === "dark" ? <SunIcon /> : <MoonIcon />}
        </Button>
      </header>
      <main className="flex flex-1 items-start justify-center px-4 pt-[8vh] pb-10">
        <div className="w-full max-w-sm">
          <div className="mb-8 flex items-center gap-3">
            <span className="grid size-11 place-items-center rounded-lg bg-primary text-primary-foreground">
              <SkepMark className="size-7" />
            </span>
            <div>
              <h1 className="text-xl font-semibold tracking-tight">Skep</h1>
              <p className="text-sm text-muted-foreground">Log in to manage this host</p>
            </div>
          </div>

          {why ? (
            <Notice tone="info" title={why.title} className="mb-5">
              {why.text || null}
            </Notice>
          ) : null}

          <form onSubmit={submit} className="space-y-4" aria-label="Log in" noValidate>
            <div className="space-y-1.5">
              <Label htmlFor="login-username">Username</Label>
              <Input
                id="login-username"
                name="username"
                autoComplete="username"
                autoCapitalize="none"
                autoCorrect="off"
                spellCheck={false}
                required
                autoFocus
                value={username}
                onChange={(e) => setUsername(e.target.value)}
                disabled={busy}
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="login-password">Password</Label>
              <Input
                id="login-password"
                name="password"
                type="password"
                autoComplete="current-password"
                required
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                disabled={busy}
              />
            </div>

            {failure ? (
              <Notice tone="bad" title="Could not log in">
                <p>{failure.text}</p>
                {failure.command ? (
                  <pre className="mt-2 overflow-x-auto rounded-md bg-muted px-3 py-2 font-mono text-xs">{failure.command}</pre>
                ) : null}
              </Notice>
            ) : null}

            <Button type="submit" className="w-full" disabled={busy || waiting || username.trim() === "" || password === ""}>
              {busy ? "Logging in…" : waiting ? `Wait ${secondsLeft}s` : "Log in"}
            </Button>
            <FieldText>
              Sessions end after a period without activity and at a fixed time after the login, both set by the
              administrator of this host.
            </FieldText>
          </form>
        </div>
      </main>
    </div>
  );
}
