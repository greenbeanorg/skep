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

The agent has no CORS headers, so the dev server proxies `/api` to it.

```sh
make run-dev                 # in the repository root: agent on 127.0.0.1:8765
cd web
npm ci
npm run dev                  # http://127.0.0.1:5173
```

Set `SKEP_API_URL` to proxy to another address.

```sh
npm run typecheck
npm test                     # unit tests: formatting and the API client
npm run build                # type-check, then build into dist/
```

Node.js: the major in `.node-version` (26). CI reads the same file.

## Dependency audit

`npm audit` on 2026-11-12, with the committed lockfile:

- Production dependencies (`npm audit --omit=dev`): **0 vulnerabilities**.
- Development dependencies: 3 findings, all in the test runner. Nothing
  was upgraded.

  | Package | Severity | Advisory |
  |---|---|---|
  | `tinypool` 1.1.1 (via `vitest`) | critical | GHSA-5gmw-xhrv-c9v3, GHSA-85c8-ppgw-ccpr: prototype pollution gadget leading to code execution |
  | `@vitest/mocker` 3.2.7 (via `vitest`) | moderate | GHSA-82fw-gwwq-j7x9, GHSA-qxg6-89mg-rqv8: file read and code injection through mock registration |
  | `vitest` 3.2.7 | critical | only because it depends on the two above |

  The only fix npm offers is `vitest@5`, a jump of two majors. These
  packages run only during `npm test`; they are not in the built files.
  Upgrading is a separate change to make and verify deliberately.

`npm` also reports that `esbuild`'s postinstall script was not run (npm's
install-script allow-list). The build and the tests work without it.

## Decisions made in this scaffold, for review

- **Dependency versions are pinned to majors one behind the newest**
  (Vite 7, TypeScript 5, React Router 7, Vitest 3): the scaffold was first
  written without a compiler, against the versions its author knew.
  Upgrading is a separate, verified step; the Vitest upgrade is the one
  with a reason to do soon (see the audit above).
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
