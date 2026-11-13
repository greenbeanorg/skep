import { useIdentity } from "@/app/session";
import { useSystem } from "@/app/system";
import { THEME_CHOICES, useTheme, type ThemeChoice } from "@/app/theme";
import { ErrorNotice } from "@/components/errors";
import { Fact, Facts, LoadingCards, PageHeader, RefreshButton } from "@/components/page";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Notice } from "@/components/ui/notice";
import { formatDateTime, formatDuration } from "@/lib/format";
import { cn } from "@/lib/utils";

const THEME_LABEL: Record<ThemeChoice, string> = { light: "Light", dark: "Dark", system: "Follow the system" };

function ThemeChooser() {
  const { choice, setChoice } = useTheme();
  return (
    <div role="radiogroup" aria-label="Theme" className="flex flex-wrap gap-1.5">
      {THEME_CHOICES.map((c) => (
        <button
          key={c}
          type="button"
          role="radio"
          aria-checked={choice === c}
          onClick={() => setChoice(c)}
          className={cn(
            "rounded-full border px-3 py-1 text-xs font-medium transition-colors",
            choice === c ? "border-transparent bg-accent text-accent-foreground" : "bg-surface hover:bg-muted",
          )}
        >
          {THEME_LABEL[c]}
        </button>
      ))}
    </div>
  );
}

/** About this node: version, certificate, session lifetimes, and the theme. */
export function SettingsPage() {
  const system = useSystem();
  const { session } = useIdentity();
  const sys = system.data;
  const tls = sys?.api?.tls;
  const expiresSoon = tls ? Date.parse(tls.not_after) - Date.now() < 30 * 86_400_000 : false;

  return (
    <>
      <PageHeader
        title="Settings"
        description="About this node. Settings are changed in the configuration files on the host in this release."
        actions={<RefreshButton onClick={system.reload} updatedAt={system.updatedAt} />}
      />
      {system.error ? <ErrorNotice error={system.error} onRetry={system.reload} stale={sys !== undefined} /> : null}
      {system.loading ? <LoadingCards count={3} /> : null}
      {sys ? (
        <div className="grid gap-4 lg:grid-cols-2">
          <Card>
            <CardHeader>
              <div>
                <CardTitle>Skep</CardTitle>
                <CardDescription>The build the agent on this node runs</CardDescription>
              </div>
            </CardHeader>
            <CardContent>
              <Facts>
                <Fact label="Version">{sys.skep.version}</Fact>
                <Fact label="Commit" mono>
                  {sys.skep.commit}
                </Fact>
                <Fact label="Built">{sys.skep.build_date}</Fact>
                <Fact label="Go">{sys.skep.go_version}</Fact>
                <Fact label="Target">
                  {sys.skep.os}/{sys.skep.arch}
                </Fact>
                <Fact label="Node ID" mono>
                  {sys.node_id}
                </Fact>
              </Facts>
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <div>
                <CardTitle>API certificate</CardTitle>
                <CardDescription>What skep-api serves on its TLS listener</CardDescription>
              </div>
              {tls ? <Badge tone={tls.source === "operator" ? "ok" : "neutral"}>{tls.source}</Badge> : null}
            </CardHeader>
            <CardContent>
              {tls ? (
                <>
                  <Facts>
                    <Fact label="Source">
                      {tls.source === "self-signed"
                        ? "Generated and renewed by skep-api, always with the same key"
                        : "Supplied by the operator; Skep does not renew it"}
                    </Fact>
                    <Fact label="Expires">
                      {formatDateTime(tls.not_after)}
                      {expiresSoon ? <Badge tone="warn" className="ml-2">within 30 days</Badge> : null}
                    </Fact>
                    <Fact label="Public key pin" mono>
                      {tls.public_key_sha256}
                    </Fact>
                  </Facts>
                  <Notice tone="neutral" className="mt-4">
                    The pin is what <span className="font-mono">skepctl</span> compares and what{" "}
                    <span className="font-mono">skep-api tls fingerprint</span> prints on the host. Seeing it here
                    proves nothing by itself: a man in the middle would show its own page. Compare it on the host
                    before trusting a new certificate.
                  </Notice>
                </>
              ) : (
                <p className="text-sm text-muted-foreground">The server did not report its certificate.</p>
              )}
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <div>
                <CardTitle>Sessions</CardTitle>
                <CardDescription>Lifetimes, from the server's configuration</CardDescription>
              </div>
            </CardHeader>
            <CardContent>
              <Facts>
                <Fact label="Idle timeout">{formatDuration(session.idle_timeout_seconds)} without activity</Fact>
                <Fact label="Your session's end">{formatDateTime(session.expires_at)}</Fact>
              </Facts>
              <p className="mt-3 text-xs text-muted-foreground">
                The absolute limit is set in <span className="font-mono">auth.session.absolute_timeout</span> and the
                idle timeout in <span className="font-mono">auth.session.idle_timeout</span> of api.json. Automatic
                refreshes of this interface do not count as activity.
              </p>
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <div>
                <CardTitle>Appearance</CardTitle>
                <CardDescription>Kept in this browser only</CardDescription>
              </div>
            </CardHeader>
            <CardContent>
              <ThemeChooser />
            </CardContent>
          </Card>
        </div>
      ) : null}
    </>
  );
}
