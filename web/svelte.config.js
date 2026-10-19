import adapter from '@sveltejs/adapter-static';
import { vitePreprocess } from '@sveltejs/vite-plugin-svelte';

/** @type {import('@sveltejs/kit').Config} */
export default {
	preprocess: vitePreprocess(),

	// Runes only, here as well as in vite.config.ts: the build reads the Vite plugin's options,
	// but svelte-check and eslint-plugin-svelte read this file. Without it a component that
	// uses no runes compiles in legacy mode, where `export let` is valid.
	compilerOptions: { runes: true },

	// SPA mode: the Go binary serves these assets from embed.FS and routes unmatched non-/api
	// paths to the fallback. No Node in production.
	//
	// Output goes to build/app, one level below what Go embeds. The adapter empties its own
	// output directory on every build, and `//go:embed` needs the embedded directory to exist
	// at compile time, so the placeholder that keeps `build/` in a fresh checkout lives where
	// the adapter does not sweep.
	kit: {
		adapter: adapter({ pages: 'build/app', assets: 'build/app', fallback: 'index.html' })
	}
};
