import adapter from '@sveltejs/adapter-static';
import { vitePreprocess } from '@sveltejs/vite-plugin-svelte';

/** @type {import('@sveltejs/kit').Config} */
export default {
	preprocess: vitePreprocess(),

	// `↯` F1 lives here, not only in vite.config.ts. The Vite plugin's compilerOptions are
	// read by the *build*; svelte-check and eslint-plugin-svelte read this file. Without it
	// a component that uses no runes is compiled in legacy mode, where `export let` is
	// perfectly valid — so both checkers accepted it and only the build would have
	// complained. Found by negative-controlling the guard (ADR-002, `06 §4`).
	compilerOptions: { runes: true },

	// SPA mode (ADR-002): the Go binary serves these assets from embed.FS and routes
	// unmatched non-/api paths to the fallback (06 §4, 11 §8.2). No Node in production.
	//
	// `↯` Output goes to build/app, one level below what Go embeds. The adapter empties its
	// own output directory on every build, and `//go:embed` needs the embedded directory to
	// exist at compile time — so the placeholder that keeps `build/` in a fresh checkout has
	// to live somewhere the adapter does not sweep (ADR-092).
	kit: {
		adapter: adapter({ pages: 'build/app', assets: 'build/app', fallback: 'index.html' })
	}
};
