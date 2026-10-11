import { defineConfig } from 'vitest/config';
import { sveltekit } from '@sveltejs/kit/vite';
import tailwindcss from '@tailwindcss/vite';
import { svelteTesting } from '@testing-library/svelte/vite';

export default defineConfig({
	// The adapter and compilerOptions live in svelte.config.js so that svelte-check and
	// eslint see them too — see the note on runes there.
	plugins: [tailwindcss(), sveltekit()],

	// `make dev` runs this alongside the daemon, so the dev server has to hand /api and the
	// socket to it — same-origin, because the panel sends no CORS headers under any
	// configuration (D3, ADR-036) and the WebSocket upgrade requires a matching Origin
	// (`11 §6.3`). VALMIN_SERVER_LISTEN's default is :8080.
	server: {
		proxy: {
			'/api': {
				target: process.env.VALMIN_DEV_PANEL ?? 'http://localhost:8080',
				ws: true,
				changeOrigin: false
			}
		}
	},
	test: {
		expect: { requireAssertions: true },
		projects: [
			{
				extends: './vite.config.ts',
				test: {
					name: 'server',
					environment: 'node',
					include: ['src/**/*.{test,spec}.{js,ts}'],
					exclude: ['src/**/*.svelte.{test,spec}.{js,ts}']
				}
			},
			// Screens rendered in a DOM and driven the way an operator drives them, against a
			// fake daemon behind fetch. A behaviour is asserted here, where it can be seen to
			// happen, rather than by matching the source that is meant to produce it.
			{
				extends: './vite.config.ts',
				plugins: [svelteTesting()],
				test: {
					name: 'client',
					environment: 'jsdom',
					include: ['src/**/*.svelte.{test,spec}.{js,ts}'],
					setupFiles: ['src/lib/testing/setup-client.ts']
				}
			}
		]
	}
});
