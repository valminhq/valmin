import adapter from '@sveltejs/adapter-static';
import { vitePreprocess } from '@sveltejs/vite-plugin-svelte';
import { defineConfig } from 'vitest/config';
import { sveltekit } from '@sveltejs/kit/vite';
import tailwindcss from '@tailwindcss/vite';
import { svelteTesting } from '@testing-library/svelte/vite';

export default defineConfig({
	plugins: [
		tailwindcss(),
		sveltekit({
			preprocess: vitePreprocess(),

			// Runes only. Without it a component that uses no runes compiles in legacy mode,
			// where `export let` is valid. svelte-check reads this option; eslint-plugin-svelte
			// does not.
			compilerOptions: { runes: true },

			// SPA mode: the Go binary serves these assets from embed.FS and routes unmatched
			// non-/api paths to the fallback. No Node in production.
			//
			// Output goes to build/app, one level below what Go embeds. The adapter empties its
			// own output directory on every build, and `//go:embed` needs the embedded directory
			// to exist at compile time, so the placeholder that keeps `build/` in a fresh checkout
			// lives where the adapter does not sweep.
			adapter: adapter({
				pages: 'build/app',
				assets: 'build/app',
				fallback: 'index.html'
			})
		})
	],
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
