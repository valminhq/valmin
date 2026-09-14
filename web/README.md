# Valmin frontend

The panel UI is a SvelteKit SPA built with Svelte 5, TypeScript, and Tailwind CSS.
Its production build is embedded in the Go daemon.

For the full setup, see [the project README](../docs/development.md). Run
`make dev` from the repository root to start Vite and the daemon together, then
open `http://localhost:5173`.

To run only the frontend against an existing development daemon:

```sh
npm ci
npm run dev -- --strictPort
```

Vite forwards `/api`, including WebSockets, to `http://localhost:8080`. Set
`VALMIN_DEV_PANEL` to change that target. The daemon's
`VALMIN_SERVER_EXTERNAL_URL` must match the browser origin.

Commands from this directory:

| Command         | Purpose                           |
| --------------- | --------------------------------- |
| `npm test`      | Run frontend unit tests.          |
| `npm run check` | Run Svelte and TypeScript checks. |
| `npm run lint`  | Check formatting and run ESLint.  |
| `npm run build` | Build the static SPA.             |

Use `make build` from the repository root to include frontend changes in
`bin/valmind`.
