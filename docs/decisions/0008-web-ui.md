# ADR 0008: The first web interface (Milestone 5)

- Status: **Accepted**, implemented 2026-11-13 (see
  [Implementation notes](#implementation-notes)). Every recommendation was
  accepted; the review answers and one amendment are under
  [Decisions](#decisions) and are worked into the text.
- Date: 2026-11-13 (proposed and accepted)
- Depends on: [ADR 0004](0004-privilege-boundary.md) (unprivileged
  `skep-api`), [ADR 0006](0006-api-split-authentication-tls.md) (cookie
  sessions, the origin check, TLS 1.3, RBAC), [ADR 0007](0007-retention.md)
  (health checks the UI shows).

## Summary

Milestone 5 delivers the first web interface. It is a client of the public
API and nothing else: everything it shows or does goes through
`/api/v1`, with the same authentication, permissions, and audit as
`skepctl`. It adds no privileged path and no endpoint that only the UI
uses, with one small exception (Decision 3).

| # | Decision | Recommendation |
|---|---|---|
| 1 | Pages and navigation | All twelve sections in the navigation; only sections the API can fill have pages, the rest say which milestone brings them; entries are filtered by permission |
| 2 | Login and session | Cookie session only (ADR 0006); the UI learns who is logged in from `GET /auth/session` and keeps nothing about the session in browser storage |
| 3 | Idle timeout and background refresh | Background refreshes send `X-Skep-Background: 1`, which `skep-api` authenticates but does not count as activity |
| 4 | Serving, in production and in development | `skep-api` serves the built files from the same origin, embedded in the binary; in development Vite serves over HTTPS with the dev certificate and proxies `/api` to `skep-api` |
| 5 | Errors and an expired session | One API client maps every error; a 401 returns to the login page, keeps the current address, and never repeats a write |
| 6 | Theme | Light, dark, or follow the system; the choice is kept in `localStorage`, applied before the first paint by a script file |
| 7 | What is interactive in this milestone | Login, logout, own password, task cancel, and user management; everything else read-only |
| 8 | The existing scaffold and testing | Start from `feat/web-scaffold`, fixed and rebased; component tests in Vitest and a browser smoke test in CI |

## Context

- The API exists and is authenticated: health, system, node hardware,
  tasks (read and cancel), users (CRUD), roles (read), the access audit
  log (read), login, logout, the current session, and the own password.
  Every route requires a permission or a session, and denials are audited
  (ADR 0006).
- **Sessions.** A login without `"session": "bearer"` creates a cookie
  session: `__Host-skep_session`, `Secure`, `HttpOnly`, `SameSite=Strict`,
  `Path=/`. The token never appears in a response body, so JavaScript
  never sees it. A non-GET request authenticated by cookie must carry an
  `Origin` header equal to `https://` plus the request's `Host`, or it is
  refused and audited. (Amendment a extended this, before the milestone,
  to every request that carries an `Origin` header.) Sessions end after 30 minutes idle or 12 hours, and
  `last_seen_at` is refreshed at most once a minute by any authenticated
  request.
- **The server sends** `Content-Security-Policy: default-src 'none';
  frame-ancestors 'none'`, `X-Content-Type-Options: nosniff`, and
  `Cache-Control: no-store` on every response today. HSTS only with an
  operator certificate.
- **TLS** is 1.3 only. Without an operator certificate the certificate is
  self-signed, so a browser warns on the first visit.
- **A draft scaffold exists** on the unmerged branch `feat/web-scaffold`:
  React 19, TypeScript, Vite 7, Tailwind CSS 4, shadcn/ui conventions, an
  original honey-and-straw design, an app shell with the twelve sections,
  light and dark themes, and read-only pages for the system, hardware, and
  tasks. It predates Milestone 4: it has no login, proxies to the old
  unauthenticated agent port, has an inline theme script, has never been
  opened in a browser, and its test runner (Vitest 3) has two critical
  advisories in development dependencies.
- `CLAUDE.md` names the sections: Dashboard, Cluster, Nodes, VMs,
  Containers, Storage, Networking, HA, Backups, Monitoring, Users and
  Permissions, Settings. It also says not to build speculative features.

## Decision 1: pages and navigation

| Option | Notes |
|---|---|
| A. Only the pages the API can fill | Honest and small. The shape of the product is invisible, and each later milestone reworks the navigation. |
| B. All twelve sections, each with a page; future ones a placeholder that names its milestone | The final shape is visible from the start and the navigation does not move later. Placeholders must not imitate data. |
| C. All twelve sections with mock data | Looks finished. Misleading, and the mocks would be speculative designs of APIs that do not exist. |

**Recommendation: B**, with these rules:

- **Placeholders show no data**, real or invented: the section name, one
  sentence of what it will hold, and the milestone that brings it. They
  are a single shared component, not twelve pages.
- **Entries are filtered by permission** from `GET /auth/session`. A
  viewer does not see Users or the audit log. The server is still the
  authority; a hidden entry is a convenience, not a control.
- **Addresses are real paths** (`/nodes/node-1`, `/tasks/abc`), using the
  History API, so a page can be bookmarked and reloaded. The server
  answers `index.html` for them (Decision 4).

Pages with content in this milestone:

| Section | Page | Endpoints | Permission |
|---|---|---|---|
| Dashboard | Node name, platform, version, uptime, health with every check (degraded ones highlighted), capabilities, the five newest tasks | `/system` (its `health` field), `/tasks?limit=5` | `system:read`, `task:read` |
| Nodes | The one node: hardware (CPU, memory, disks, NICs, ZFS pools) and capabilities with their reasons | `/system`, `/nodes/{id}/hardware` | `node:read` |
| Tasks (under Monitoring) | Task list with status filter and paging; task detail with its events; cancel | `/tasks`, `/tasks/{id}`, `/tasks/{id}/cancel` | `task:read`, `task:cancel` |
| Users and Permissions | Users (list, create, edit, disable, delete, set password); roles and their permissions | `/users`, `/users/{id}`, `/roles` | `user:read`, `user:manage`, `role:read` |
| Users and Permissions | Access audit log, paged | `/audit` | `audit:read` |
| Settings | About this node: version and build, the API certificate (source, expiry, key pin, certificate fingerprint), session lifetimes | `/system` | `system:read` |
| (account menu) | Own account: change password, session information, log out | `/auth/session`, `/auth/password`, `/auth/logout` | session |

Cluster, VMs, Containers, Storage (beyond the pools on the node page),
Networking, HA, and Backups are placeholders. "Tasks" lives under
Monitoring until Monitoring has metrics of its own.

## Decision 2: login and session

ADR 0006 fixes the mechanism: the UI uses a cookie session and never a
bearer token. What remains is how the UI uses it.

| Option | Notes |
|---|---|
| A. Ask the server on start and keep the identity in memory only | One request at start. Nothing about the session survives in the browser beyond the `HttpOnly` cookie. |
| B. Also cache the user and permissions in `localStorage` | Faster first paint. Stale after a role change, readable by any script on the origin, and a second source of truth. |
| C. Bearer token in memory | Contradicts ADR 0006 and amendment (a). Excluded. |

**Recommendation: A.**

- **Start:** `GET /api/v1/auth/session`. `200` shows the app, with the user,
  role, permissions, and session lifetimes held in memory. `401` shows the
  login page.
- **Login page:** username and password, `POST /api/v1/auth/login` with
  no `session` field, which gives a cookie session. On success the body
  (user, session, permissions; no token) replaces the in-memory identity.
  The page offers no "remember me": lifetimes are the server's.
- **Nothing about the session in browser storage**: no token (JavaScript
  cannot read it anyway), no user, no permissions. `localStorage` holds
  only the theme (Decision 6).
- **Logout:** `POST /api/v1/auth/logout`, then the in-memory identity is
  cleared and the login page shown. Other open tabs are told at once over
  a `BroadcastChannel` that carries only the word "logged out", and the
  same happens after a password change, which ends every session.
- **`Origin` must always be this host** (amendment a). Any request that
  carries an `Origin` header, on any route and with any credentials or
  none, including `POST /auth/login` and `POST /auth/logout`, must name
  `https://` plus the request's `Host`, or `skep-api` refuses it.
  Requests without `Origin`, such as `skepctl`'s, are unaffected, and a
  cookie-authenticated write must still carry one. Browsers add `Origin`
  to same-origin `fetch` calls that are not GET or HEAD, so the UI passes
  without doing anything, as long as it stays same-origin (Decision 4).
- **Requests use `credentials: "same-origin"`**, the `fetch` default, and
  never `"include"`.
- **First administrator:** still a local command (ADR 0006, decision 8).
  If no user exists the login answers `setup_required`, and the login page
  says which command to run on the host.

## Decision 3: idle timeout and background refresh

The dashboard and the task pages refresh themselves. Today every
authenticated request extends the idle timeout, so an open tab keeps its
session alive until the 12-hour limit even when nobody is at the screen.
That quietly defeats the 30-minute idle timeout.

| Option | Notes |
|---|---|
| A. Accept it | Nothing to build. An unattended browser stays logged in for up to 12 hours. |
| B. Mark background requests; `skep-api` authenticates them but does not refresh `last_seen_at` | A few lines in the authenticator and one header in the UI. The idle timeout means what it says. A client can only shorten its own session with it, never lengthen it. |
| C. No automatic refresh; a refresh button only | No server change. Stale status on a dashboard, which is the point of a dashboard. |
| D. Stop refreshing when the tab is hidden, otherwise as A | Helps, but a visible tab on an unattended screen is the case that matters. |

**Recommendation: B, combined with D.**

- The UI sends `X-Skep-Background: 1` on requests it makes by itself
  (timers, refetch on focus). Requests caused by the user (navigation,
  a button, a form) do not carry it.
- `skep-api` accepts such a request with the same checks as any other,
  including the idle check, and skips only the `last_seen_at` update.
  When the session has idled out, a background request gets `401` like
  any other.
- Refreshing pauses while the tab is hidden (`visibilitychange`).
- **Warning before the end.** From the session lifetimes and the time of
  the last user-initiated request, the UI shows a notice two minutes
  before the idle timeout ("Stay signed in" makes one ordinary request)
  and five minutes before the absolute limit (which cannot be extended:
  "log in again to continue"). The server decides; the notice is a
  courtesy and may be wrong by up to a minute, the touch interval.
- Documented in `api/openapi.yaml` as a request header any client may
  send. It is tested in `internal/auth` and `internal/api`.

This is the only server change the milestone needs besides serving the
files (Decision 4).

## Decision 4: serving, in production and in development

### 4a. Production

| Option | Notes |
|---|---|
| A. `skep-api` serves the built files on its TLS listener, same origin as the API | No extra component or configuration. The cookie, `SameSite=Strict`, and the origin check work unchanged. `skep-api` already runs unprivileged. |
| B. A separate web server (nginx) serves the files and reverse-proxies `/api` to `skep-api` | Same origin through the proxy. Another daemon to install, configure, and secure; `trusted_proxies` must be set for correct source addresses; TLS terminates in two places or the hop is plaintext. Useful for sites that already front services this way, and still possible with A. |
| C. A separate origin (another port or host) calling the API with CORS | Cookies need `SameSite=None` or a same-site arrangement, credentials in CORS, and the origin check must learn a list of allowed origins. Weakens ADR 0006's controls for no gain. Excluded. |

**Recommendation: A.** Operators who want B can still put a proxy in
front; nothing in A prevents it.

How the files get into `skep-api`:

| Option | Notes |
|---|---|
| A1. Embedded with `go:embed` | One binary carries the UI that matches its API; deploying to `hearst` stays one `scp`. A Go build needs the built files, or a placeholder. |
| A2. Read from a directory (`/usr/local/share/skep/web`, configurable) | The Go build stays independent of npm. The UI and the API can drift apart on a host, and the directory is one more thing to package and protect. |

**Recommendation: A1, with a placeholder.** `internal/webui` embeds
`internal/webui/dist`. The repository commits a placeholder
`index.html` there that says the web interface was not built into this
binary and how to build it, so `go build ./...` and `go test ./...` keep
working without Node.js. `make build` and `make build-freebsd` build the
UI first and copy it in; CI does the same and checks that a release
binary does not contain the placeholder.

Routing in `skep-api`:

- `/api/...` is the API, exactly as now. It never falls back to the UI: an
  unknown API path stays a JSON `404`.
- `/assets/...` are the hashed build files: `Cache-Control: public,
  max-age=31536000, immutable`, correct `Content-Type`, `nosniff`.
- Any other `GET` or `HEAD` answers `index.html` with `Cache-Control:
  no-store`, so a new release is picked up at the next load. Other methods
  get `405`.
- The UI's files are public: they contain no data and the login page must
  load without a session. They are not written to the access audit log.

Security headers for the UI's responses (the API's stay as they are):

```
Content-Security-Policy: default-src 'none'; script-src 'self'; style-src 'self';
  img-src 'self' data:; font-src 'self'; connect-src 'self';
  base-uri 'none'; form-action 'self'; frame-ancestors 'none'
Referrer-Policy: no-referrer
X-Content-Type-Options: nosniff
Cross-Origin-Opener-Policy: same-origin
```

No inline script or style is allowed, so the scaffold's inline theme
script moves to a file (Decision 6). Styles that React sets through the
DOM are not affected by `style-src`; whether any library needs more is
checked in a browser before the policy is final.

### 4b. Development

| Option | Notes |
|---|---|
| A. Vite over HTTPS with the dev certificate `skep-api` generated, proxying `/api` to `skep-api` | Same origin from the browser's view; the cookie is `Secure` and `__Host-` as in production; the origin check passes unchanged; hot reload works. |
| B. Vite over plain HTTP, proxying | The browser sends `Origin: http://...`, which the origin check refuses, and `__Host-` cookies need HTTPS. Working around either weakens the server. Excluded. |
| C. Build and let `skep-api` serve, no dev server | No hot reload; slow to work on. Fine for a final check, not for development. |

**Recommendation: A**, with C as the check before a commit.

- `vite.config.ts` reads `.skep-dev/api/tls/cert.pem` and `key.pem`
  (created by `make run-dev`) for `server.https`, listens on
  `127.0.0.1:5173`, and proxies `/api` to `https://127.0.0.1:8443`,
  verifying `skep-api` with that same certificate. It keeps the `Host`
  header (`changeOrigin: false`), so `Origin` and `Host` agree.
- The browser is told to trust the dev certificate once, as an operator
  would with a self-signed host.
- No development switch in `skep-api` relaxes the cookie, the origin
  check, or the CSP.

### 4c. The browser and the self-signed certificate

The UI cannot help with the first warning: it is not loaded until the
user accepts or the operator installs a trusted certificate. The
installation guide gets a section: compare the certificate SHA-256 the
browser shows with what `skep-api tls fingerprint` prints, or install an
operator certificate. The Settings page shows the same values for
reference, which cannot prove anything on its own, because a
man-in-the-middle would serve its own page as well. The guide says that.

## Decision 5: errors and an expired session

One API client wraps `fetch`; no page calls `fetch` itself.

| Situation | What the UI does |
|---|---|
| `401 unauthenticated` (expired, revoked, logged out elsewhere) | Clears the in-memory identity, remembers the current address, shows the login page with "Your session has ended". After login it returns to that address. A write that got `401` is **not** repeated: the user sees the form again with what they entered (except passwords) and submits it again. |
| `403 forbidden` | Shown where the action was, "Your role does not allow this", with the request ID. The UI hides actions the role lacks, so this means the role changed underneath; the next `GET /auth/session` refreshes permissions (and a role change ends sessions anyway). |
| `403` from the origin check | Should not happen from the UI; shown as an internal error with the request ID. |
| `429 rate_limited` (login) | "Too many attempts; wait N seconds", from `Retry-After`. The button stays disabled until then. |
| `invalid_credentials` | One message for a wrong username or password, as the API does. |
| `400 invalid_request` and domain errors (`username_taken`, `last_admin`, `not_cancellable`, ...) | Shown next to the form or action, with the API's message. |
| `404 not_found` | A not-found page inside the app shell. |
| `503 agent_unavailable` | A banner across the app: "The host agent is not reachable". Pages keep the last data they had, marked with when it was fetched. Background refresh continues, so the banner goes when the agent is back. |
| Network failure (no response) | A banner: "skep-api cannot be reached", the same stale-data rule, and a retry with backoff for background requests only. |
| `500 internal_error` and anything unexpected | A generic message and the request ID, which is what an operator searches the log for. No raw body, no stack. |
| Health `degraded` | Not an error: the dashboard lists the failing checks and the app shell shows a small indicator linking there. |

Rules: background requests never pop up a dialog; only the banners and
the session handling react to them. Writes are never retried
automatically. Every error shown to the user includes the request ID
when there is one.

## Decision 6: theme

| Option | Notes |
|---|---|
| A. Follow the system only (`prefers-color-scheme`) | Nothing to store. No way to choose. |
| B. Light, dark, or system; kept in `localStorage` | The common expectation. A display preference, not sensitive. Per browser, not per user. |
| C. Stored per user on the server | Follows the user between browsers. Needs a preferences API that does not exist; not worth it for one setting. |

**Recommendation: B, defaulting to system.**

- A small script file, loaded synchronously in `<head>`, reads the stored
  choice and sets `data-theme` on `<html>` before the first paint; with
  the CSP above it cannot be inline. Reading storage is wrapped so a
  private window or blocked storage falls back to system.
- Colours are CSS variables, defined for both themes, with status colours
  separate from the brand colour, and text contrast meeting WCAG AA in
  both. The design is the scaffold's own (a skep is a straw beehive), not
  modelled on any other product's interface.
- Desktop first, usable down to tablet width; a phone layout is not a
  goal of this milestone.

## Decision 7: what is interactive in this milestone

| Option | Notes |
|---|---|
| A. Read-only, plus login, logout, and the own password | Smallest risk. Users must still be managed with `skepctl` or `curl`. |
| B. A, plus the writes the API already has: cancel a task, manage users | Uses only endpoints that already have permissions, audit, and tests (including last-administrator protection). The UI becomes useful for administration. |
| C. B, plus new writes (configuration, discovery runs, and so on) | Each needs an API, permissions, and audit first; that is other milestones' work. Excluded. |

**Recommendation: B.**

Interactive:

- Log in, log out, change the own password (which ends every session and
  returns to the login page, saying why).
- Cancel a task (`task:cancel`), with a confirmation that names the task.
- Users (`user:manage`): create; change display name and role; disable
  and enable; set a password; delete. Disabling, deleting, a role change,
  and a password change each end that user's sessions, and the
  confirmation says so. Deleting asks for the username to be typed.
  `last_admin` refusals are shown as such.

Read-only: the dashboard, the node and its hardware, task detail, roles,
the audit log, and Settings. There is no role editing (ADR 0006,
decision 3) and no VM, storage, network, or jail action of any kind.

## Decision 8: the existing scaffold and testing

| Option | Notes |
|---|---|
| A. Start from `feat/web-scaffold`: rebase onto `main`, then fix it | The design, shell, theme tokens, API client, and pages exist and build. Known faults to fix first: no login, the proxy targets the old agent port over HTTP, the inline theme script, Vitest 3 with critical development advisories. |
| B. Start again | Nothing gained; the scaffold's choices match the approved stack. |

**Recommendation: A.** Before any feature work: rebase, upgrade Vitest to
a version without the advisories, point the dev proxy at `skep-api` over
HTTPS, move the theme script to a file, and open every page in a browser
in both themes. The scaffold's "decisions for review" are settled here:
no data-fetching library yet (the hook gains polling, the background
header, and cancellation; a library is reconsidered if caching across
pages becomes a real need), local icons, and API types generated from
`api/openapi.yaml` (`openapi-typescript`, a development dependency) so
drift fails the build.

Testing:

| Layer | Tool | What |
|---|---|---|
| Unit | Vitest | API client error mapping, the session state machine, the idle-warning timing, formatting |
| Component | Vitest with Testing Library and jsdom | Login page, permission filtering of the navigation, confirmations, error banners |
| Browser | Playwright, in CI, against a real `skep-api` built with the UI and a fake agent | Log in, dashboard renders, a viewer cannot see Users, cancel a task, log out, an expired session returns to the login page and back, the CSP produces no violations |
| Server | Go | Static file routes, fallback, headers, the background header |
| FreeBSD | `hearst` | `skep-api` from the FreeBSD build serves the UI to a browser on the lab network; pending FreeBSD validation until done |

Testing Library and Playwright are new development dependencies. Neither
ships in the built files. Installing Playwright's browsers and system
dependencies on `merrick` (`npx playwright install --with-deps`) is
approved for when the build starts; if the lab's DNS filtering blocks the
download, the work stops and it is raised, never worked around.

## Consequences

- `skep-api` grows a small static-file handler and one header rule in the
  authenticator. Its public surface gains static, data-free files.
- Release builds need Node.js. Plain Go builds and tests do not.
- The UI is exactly as capable as the API and the user's role allow, and
  every action it takes is in the access audit log like any other client's.
- Placeholders make the remaining milestones visible without promising
  anything they cannot show.

## Suggested implementation order

1. Rebase and repair the scaffold (Decision 8, first paragraph).
2. `internal/webui` with the placeholder, routes, and headers; Makefile
   and CI steps (Decision 4).
3. The background header in `skep-api` (Decision 3).
4. API client, session state, login page, logout, error handling
   (Decisions 2 and 5).
5. Read-only pages, then the interactive ones (Decisions 1 and 7).
6. Browser tests; documentation; validation on `hearst`.

## Decisions

Accepted on 2026-11-13 with these answers to the six review questions.

1. **All twelve sections, with placeholders** for those the API cannot
   fill yet (Decision 1).
2. **Add `X-Skep-Background`**, so that an unattended tab idles out
   (Decision 3).
3. **Embedded with `go:embed`, with the placeholder** for builds without
   Node.js (Decision 4a, option A1).
4. **Include user management and task cancel** (Decision 7, option B).
5. **Playwright and Testing Library as development dependencies**
   (Decision 8). Installing Playwright's browsers and system
   dependencies on `merrick` is approved for the build; a download
   blocked by the lab's DNS filtering stops the work and is raised.
6. **Build on `feat/web-scaffold`** (Decision 8, option A).

One amendment was made at acceptance and is worked into Decision 2.

- **a. The origin check covers every request.** Any request that carries
  an `Origin` header must match `https://` plus the request's `Host`,
  including `POST /auth/login` and `POST /auth/logout`, not only
  cookie-authenticated writes. Requests without `Origin`, such as
  `skepctl`'s, are unaffected. It is a change to `skep-api`, not to the
  UI, so it was implemented at once, before the milestone, in its own
  change (#18), and is recorded in the implementation notes of
  [ADR 0006](0006-api-split-authentication-tls.md). The proposal relied
  on the existing check, which covered only cookie-authenticated writes
  and the login.

## Implementation notes

Implemented on 2026-11-13 on the branch `feat/web-ui`, in the suggested
order. Where the implementation differs from the text above, the
implementation is what holds:

- **Embedding (decision 4a).** The text says the repository commits a
  placeholder `index.html` inside the embedded directory. Instead the
  built files are embedded behind the build tag `skep_webui`
  (`internal/webui/embed_dist.go`), and a build without the tag embeds a
  committed placeholder page from `internal/webui/placeholder/`
  (`embed_placeholder.go`). `internal/webui/dist` is never committed and
  `make build` never dirties the working tree, which a placeholder
  overwritten by the build would have. The effect the decision asked for
  is the same: `go build ./...` and `go test ./...` need no Node.js, and
  CI checks that the FreeBSD `skep-api` built by `make build-freebsd`
  carries the interface and not the placeholder. `make build-go` and
  `make build-freebsd-go` are the Go-only builds; `make run-dev` uses the
  former, because in development the interface is served by Vite.
- **Routing (decision 4a).** A path whose last segment looks like a file
  name (has an extension) and does not exist is a 404, not `index.html`,
  so a missing file is reported as missing. A path with a hidden segment
  (starting with a dot) is a 404 too.
- **Navigation (decision 1).** "Tasks" and the "Metrics" placeholder form
  a "Monitoring" group; the twelve sections are all present.
- **The background header (decision 3)** is `internal/auth`'s
  `AuthenticateBackground`, selected by `internal/api` when
  `X-Skep-Background` has any non-empty value, and is documented as a
  reusable parameter in `api/openapi.yaml`.
- **Shared data (decision 8).** The one resource several pages show,
  `/system`, has a provider that polls it once for the shell and the
  pages; `useQuery` gained the background header, pausing while the tab
  is hidden, backoff while `skep-api` is unreachable, and cancellation.
  No data-fetching library was added.
- **Testing (decision 8).** Vitest 5 (Vitest 3's advisories are gone),
  Testing Library with jsdom for the login page and the user dialogs,
  and Playwright against a real `skep-api` with the interface embedded
  and the agent's test build (`scripts/e2e-stack.sh`), run by CI's
  release job. The Playwright run also photographs every page in both
  themes.
- **Validation on `hearst`** is recorded in the architecture overview's
  FreeBSD validation table.

