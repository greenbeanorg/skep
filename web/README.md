# Skep web interface

The first web interface, Milestone 5
([ADR 0008](../docs/decisions/0008-web-ui.md)). React 19, TypeScript, Vite
7, Tailwind CSS 4, and shadcn/ui conventions, with an original design. For
what it does and how to open it, see
[docs/administration/web-ui.md](../docs/administration/web-ui.md).

It is a client of `/api/v1` and nothing else. `skep-api` serves the built
files from the same origin as the API, embedded in the binary
(`internal/webui`), with a Content-Security-Policy that allows no inline
script or style.

## Layout

```
src/app/         App and routes, session state, navigation model, theme, connectivity, the shared /system poll
src/layouts/     AppShell: navigation, top bar, account menu, banners, the session notice
src/pages/       one file per page; users/ holds the user dialogs, roles, and the audit log
src/components/  shared components; ui/ holds shadcn-style primitives (hand-written)
src/api/         generated API types (schema.d.ts), short names (types.ts), the client, the useQuery hook
src/lib/         formatting and class-name helpers
src/test/        Vitest setup
e2e/             Playwright browser tests
public/theme.js  applies the stored theme before the first paint (no inline script under the CSP)
```

Design: a skep is a straw beehive. The palette is honey and straw on paper
(light) or on blue-black (dark); the mark is a hive; the icons are drawn
for this project (`src/components/icons.tsx`). Status colours are kept
apart from the brand colour. The tokens are CSS variables in
`src/index.css`, defined for both themes, selected by `data-theme` on
`<html>`.

## Develop

In development Vite serves the interface over HTTPS with the certificate
`skep-api` generated for itself and proxies `/api` to `skep-api` over
HTTPS, verified with that certificate and with the `Host` header kept. The
browser sees one HTTPS origin, so the session cookie (`Secure`, `__Host-`)
and the origin check work as in production. There is no plain-HTTP mode:
it would need the server to relax the cookie or the origin check.

```sh
make run-dev                 # in the repository root: agent and skep-api (https://127.0.0.1:8443)
bin/skep-api admin create --username andy -dev   # once; there are no default credentials
cd web
npm ci
npm run dev                  # https://127.0.0.1:5173
```

Tell the browser to trust the development certificate once; compare its
fingerprint with `bin/skep-api tls fingerprint -dev`. `SKEP_API_URL`
proxies to another `skep-api`; `SKEP_DEV_TLS_DIR` names another certificate
directory.

```sh
npm run typecheck
npm run check-types          # src/api/schema.d.ts matches api/openapi.yaml
npm run generate-types       # regenerate it after an API change
npm test                     # unit and component tests (Vitest, jsdom, Testing Library)
npm run build                # type-check, then build into dist/
```

Node.js: the major in `.node-version`. CI reads the same file.

## Build into skep-api

```sh
make build            # builds the interface, copies it to internal/webui/dist, builds the binaries with -tags skep_webui
make build-freebsd    # the same for FreeBSD amd64
make build-go         # Go only: the binaries carry a placeholder page instead of the interface
```

`internal/webui/dist` is never committed. CI builds the release binaries
and checks that the FreeBSD `skep-api` carries the interface and not the
placeholder.

## Browser tests

```sh
npx playwright install --with-deps chromium   # once
make web-e2e                                   # in the repository root
```

`make web-e2e` builds everything and runs Playwright against a real
`skep-api` with the interface embedded, in front of the agent's test build
(which submits one cancellable `test.sleep` task), in a temporary
directory (`scripts/e2e-stack.sh`). The tests log in, render every page in
both themes, check that a viewer cannot see Users, cancel a task, log out,
end a session underneath an open page, and assert that the CSP produces no
violation. Screenshots of every page in both themes land in
`test-results/screens/`.

`skep-api`'s certificate is trusted by its public key pin, never by
ignoring certificate errors (ADR 0010 amendment x). `make web-e2e` runs
`scripts/web-e2e.sh`, which makes the stack's key first (`skep-api tls
regenerate`), takes the pin from `skep-api tls fingerprint`, and starts
Playwright with `SKEP_E2E_PIN` (Chromium gets
`--ignore-certificate-errors-spki-list`) and `NODE_EXTRA_CA_CERTS` set to
the certificate carrying that key (Playwright's Node-side `request`
client verifies against it). `npx playwright test` on its own refuses to
start. `e2e/tls-pin.spec.ts` checks that Chromium refuses the server
without the pin and with another key's pin.

Against a `skep-api` that already runs (for example on `hearst`, with the
agent's test build behind it):

```sh
SKEP_E2E_EXTERNAL=1 SKEP_E2E_LISTEN=10.79.20.170:8443 \
  SKEP_E2E_PIN=sha256:<from skep-api tls fingerprint on that host> \
  ./scripts/web-e2e.sh
```

The wrapper fetches the served certificate, refuses it unless its key
matches the pin (`scripts/e2e-trust.mjs`), and only then trusts it.

Against a host that runs the packaged agent (not its test build), only
some specs apply: `e2e/tls-pin.spec.ts`, `e2e/clone-note.spec.ts` (with
its `SKEP_E2E_CLONE_*` variables), and the read-only headers test of
`e2e/ui.spec.ts` (`-g 'served with its headers'`). The rest of
`ui.spec.ts` needs the local stack: the `test.sleep` task, the pool
`devpool`, the administrator `andy`, and it creates the user `vera` with
a fixed password, so it is not pointed at a real host. This is how
hearst ran it on 2026-11-21 (run 4, section 29 of
`tests/integration/jails/results/step10-progress.md`).

## Dependency audit

`npm audit` on 2026-11-13, with the committed lockfile: **0
vulnerabilities**, in production and development dependencies. The test
runner was upgraded from Vitest 3 to Vitest 5 for this (the two critical
advisories in Vitest 3's `tinypool` and `@vitest/mocker` are gone).

`npm` reports that `esbuild`'s postinstall script was not run (npm's
install-script allow-list). The build and the tests work without it.

## Decisions

- **No data-fetching library.** `useQuery` (about 100 lines) handles
  polling with the background header, pausing while the tab is hidden,
  backoff while `skep-api` is unreachable, and cancellation. The one
  resource shared between pages, `/system`, has a provider. A library is
  reconsidered when caching across pages becomes a real need (ADR 0008,
  decision 8).
- **Local icons**, no icon library.
- **API types are generated** from `api/openapi.yaml` with
  `openapi-typescript` and committed, so drift fails CI.
- **Dependency versions**: Vite 7, TypeScript 5, React Router 7, Vitest 5.
  Vite 8 and TypeScript 7 exist and were not taken: each is a separate,
  verified step.
- **No linter configuration yet.** Type-checking is strict
  (`noUncheckedIndexedAccess`, unused locals and parameters are errors).

## Known limitations

- Desktop first. It is usable down to tablet width; a phone layout is not
  a goal of this milestone.
- The access audit log is the live table only; archived entries are read
  on the host (`skep-api audit export`).
- The interface has been checked in Chromium only (the Playwright tests
  and their screenshots, on the development machine and against the
  FreeBSD build on `hearst`); other browsers have not been checked.
