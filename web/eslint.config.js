import prettier from 'eslint-config-prettier';
import path from 'node:path';
import js from '@eslint/js';
import svelte from 'eslint-plugin-svelte';
import { defineConfig, includeIgnoreFile } from 'eslint/config';
import globals from 'globals';
import ts from 'typescript-eslint';

const gitignorePath = path.resolve(import.meta.dirname, '.gitignore');

export default defineConfig(
	includeIgnoreFile(gitignorePath),
	js.configs.recommended,
	ts.configs.recommended,
	svelte.configs.recommended,
	prettier,
	svelte.configs.prettier,
	{
		languageOptions: { globals: { ...globals.browser, ...globals.node } },
		rules: {
			// typescript-eslint strongly recommend that you do not use the no-undef lint rule on TypeScript projects.
			// see: https://typescript-eslint.io/troubleshooting/faqs/eslint/#i-get-errors-from-the-no-undef-rule-about-global-variables-not-being-defined-even-though-there-are-no-typescript-errors
			'no-undef': 'off',
			'@typescript-eslint/no-unused-vars': [
				'error',
				{ argsIgnorePattern: '^_', varsIgnorePattern: '^_' }
			]
		}
	},
	{
		files: ['**/*.svelte', '**/*.svelte.ts', '**/*.svelte.js'],
		languageOptions: {
			parserOptions: {
				projectService: true,
				extraFileExtensions: ['.svelte'],
				parser: ts.parser
			}
		},
		rules: {
			// F1 is not enforced here. Three selectors for `export let`, `$:` and
			// `$$props` were written, and all three turned out to be dead: with
			// `runes: true` in svelte.config.js the compiler rejects every one of them and
			// svelte-check reports it, while eslint-plugin-svelte does not read that option
			// and has no settings key that makes it (tried). Keeping rules that cannot fire
			// would be worse than not having them — see src/lib/lint-guard.test.ts, which
			// is where the invariant is actually held.
			'svelte/valid-compile': 'error',
			'svelte/button-has-type': 'error',
			'svelte/no-useless-mustaches': 'error',
			'svelte/require-each-key': 'error'
		}
	},
	{
		// The shadcn-svelte components are copied in, not depended on (ADR-002), and are
		// reconciled by re-running the CLI rather than edited. Holding upstream's files to
		// house rules would make every re-run a merge conflict with ourselves.
		files: ['src/lib/components/ui/**'],
		rules: {
			'no-restricted-syntax': 'off',
			'svelte/button-has-type': 'off',
			'svelte/no-navigation-without-resolve': 'off',
			'@typescript-eslint/no-explicit-any': 'off'
		}
	}
);
