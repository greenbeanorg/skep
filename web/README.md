# Skep web interface

> **Draft, never built.** This scaffold was written on a machine without
> Node.js. Nothing here has been installed, type-checked, built, tested, or
> opened in a browser. Treat every file as unverified until
> `npm install && npm run typecheck && npm test && npm run build` has run.
> There is no `package-lock.json` yet for the same reason; commit the one
> the first `npm install` produces.

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
`cn` helper, `components.json`), so `npx shadcn add <component>` can add
more. The five primitives there were written by hand, not generated,
because the shadcn CLI could not be run.

## Run

The agent has no CORS headers, so the dev server proxies `/api` to it.

```sh
make run-dev                 # in the repository root: agent on 127.0.0.1:8765
cd web
npm install
npm run dev                  # http://127.0.0.1:5173
```

Set `SKEP_API_URL` to proxy to another address.

```sh
npm run typecheck
npm test                     # unit tests: formatting and the API client
npm run build                # type-check, then build into dist/
```

## Decisions made in this scaffold, for review

- **Dependency versions are pinned to majors one behind the newest**
  (Vite 7, TypeScript 5, React Router 7, Vitest 3) because the code could
  not be compiled against anything, and those are the versions it was
  written for. Upgrading is a separate, verified step.
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

- No component tests and no end-to-end tests.
- No linter configuration.
- The top bar and the pages each fetch `/system`; there is no shared cache.
- Types in `src/api/types.ts` are hand-written from `pkg/models`, not
  generated from `api/openapi.yaml`.
