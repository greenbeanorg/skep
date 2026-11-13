# Skep web interface

> **Draft.** It installs, type-checks, passes its unit tests, and builds
> (Node.js 26, 2026-11-12), and the dev server proxies the API correctly.
> **It has not been opened in a browser yet**: nobody has looked at a
> rendered page, in either theme, at any width. There are no component or
> end-to-end tests.

React, TypeScript, Vite, Tailwind CSS 4, and shadcn/ui conventions.

## What is here

- An app shell: grouped sidebar navigation, a top bar with the node's
  name, health, and a theme switch, and a content area. Light and dark
  themes; the choice is stored in the browser.
- Read-only pages against the endpoints that exist today:

  | Page | Endpoints |
  |---|---|
  | Dashboard (system) | `GET /api/v1/system`, `/health`, `/tasks?limit=5` |
  | Nodes (hardware) | `GET /api/v1/system`, `/nodes/{id}/hardware` |
  | Tasks, task detail | `GET /api/v1/tasks`, `/tasks/{id}` |

- Every other section from the navigation (Cluster, Virtual machines,
  Containers, Storage, Networking, High availability, Backups, Monitoring,
  Users and permissions, Settings) is a placeholder that says it is not
  built and which milestone it belongs to.
- No authentication and no write actions. The API has neither yet.

## Design

The design is Skep's own. A skep is a straw beehive: the palette is honey
and straw on paper (light) or on blue-black (dark), the mark is a hive, and
the icons are drawn for this project (`src/components/icons.tsx`), several
on a honeycomb hexagon. Status colours are kept apart from the brand
colour. The tokens are CSS variables in `src/index.css`.

## Layout

```
src/app/         App, routes, navigation model, theme
src/layouts/     AppShell
src/pages/       one file per page
src/components/  shared components; ui/ holds shadcn-style primitives
src/api/         API types, client, and the useQuery hook
src/lib/         formatting and class-name helpers
```

`src/components/ui/` follows shadcn/ui conventions (`cva` variants, the
`cn` helper, `components.json`). The five primitives there were written by
hand, not generated. `npx shadcn add <component>` should be able to add
more, but that has not been tried.

## Run

The interface is served by `skep-api` on the same origin as the API (ADR
0008, decision 4). In development, Vite serves it over HTTPS with the
certificate `skep-api` generated for itself and proxies `/api` to
`skep-api`, so the browser sees one HTTPS origin and the session cookie and
the origin check work as in production. There is no plain-HTTP mode.

```sh
make run-dev                 # in the repository root: agent and skep-api (https://127.0.0.1:8443)
cd web
npm ci
npm run dev                  # https://127.0.0.1:5173
```

The browser must be told to trust the development certificate once
(`.skep-dev/api/tls/cert.pem`); compare its fingerprint with
`bin/skep-api tls fingerprint -dev`. Set `SKEP_API_URL` to proxy to another
`skep-api`, and `SKEP_DEV_TLS_DIR` to use another certificate directory.

```sh
npm run typecheck
npm test                     # unit and component tests (Vitest, jsdom)
npm run build                # type-check, then build into dist/
```

Node.js: the major in `.node-version` (26). CI reads the same file.

## Dependency audit

`npm audit` on 2026-11-13, with the committed lockfile: **0
vulnerabilities**, in production and development dependencies. The test
runner was upgraded from Vitest 3 to Vitest 5 for this (the two critical
advisories in Vitest 3's `tinypool` and `@vitest/mocker` are gone).

`npm` reports that `esbuild`'s postinstall script was not run (npm's
install-script allow-list). The build and the tests work without it.

## Decisions made in this scaffold, for review

- **Dependency versions**: Vite 7, TypeScript 5, React Router 7, Vitest 5.
  Vite 8 and TypeScript 7 exist and were not taken: each is a separate,
  verified step.
- **No data-fetching library and no icon library.** `useQuery` is about 80
  lines and the icons are local. Both keep the dependency list short;
  replace `useQuery` with a library when caching across pages matters.
- **Tasks** is in the navigation although `CLAUDE.md` does not list it,
  because the API serves tasks and the page was asked for.
- **How the built files are served is not decided.** `skep-api`
  (Milestone 4) is the likely place. The small inline theme script in
  `index.html` will need a nonce or a move to a file under a strict
  Content-Security-Policy.

## Known gaps

- Never opened in a browser. No component tests and no end-to-end tests.
- No linter configuration.
- The top bar and the pages each fetch `/system`; there is no shared cache.
- Types in `src/api/types.ts` are hand-written from `pkg/models`, not
  generated from `api/openapi.yaml`.
