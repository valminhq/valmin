import { execFileSync } from 'node:child_process';
import { mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterAll, expect, it } from 'vitest';

// `↯` F1 is a review rejection, not a style preference (`06 §4`), which means the tooling
// has to reject it too — and a guard nobody has watched fail is not a guard. This writes
// each Svelte 4 idiom into the project and asserts svelte-check rejects it.
//
// svelte-check, and not eslint, because that is where the enforcement actually is: with
// `runes: true` in svelte.config.js the Svelte compiler rejects all three, and svelte-check
// reports it. `make lint` runs it. Three eslint selectors were written first and deleted —
// eslint-plugin-svelte does not read that compiler option and has no settings key that
// makes it, so the selectors either duplicated the compiler or never fired at all.
//
// `↯` The probes live inside the project and the assertion requires the output to name
// them. Written to a tmpdir, a checker fails for its own reasons and names no file — a
// green test that proves nothing, which is what the first version of this was. Each probe
// also *uses* the value it declares, because the first version was partly satisfied by an
// unused-variable rule rather than by the guard under test.
//
// Negative control: delete `compilerOptions: { runes: true }` from svelte.config.js and
// this fails on `export let`.
const idioms: Array<[name: string, script: string, markup: string]> = [
	['export-let', 'export let name = "x";', '{name}'],
	['reactive-statement', 'let x = $state(1);\n\t$: doubled = x * 2;', '{doubled}'],
	['dollar-dollar-props', 'const p = $$props;', '{p}']
];

const dir = join('src', 'lib', '__lint_probe__');

afterAll(() => rmSync(dir, { recursive: true, force: true }));

function run(command: string, args: string[]): string {
	try {
		return execFileSync(command, args, { cwd: process.cwd(), encoding: 'utf8', stdio: 'pipe' });
	} catch (err) {
		const e = err as { stdout?: string; stderr?: string };
		return `${e.stdout ?? ''}${e.stderr ?? ''}`;
	}
}

// Spawning a checker is slow and worth it once: this is the only thing standing between a
// Svelte 4 idiom and a merge.
it(
	'every Svelte 4 idiom is rejected by eslint or svelte-check (F1, ADR-002)',
	{ timeout: 120_000 },
	() => {
		mkdirSync(dir, { recursive: true });
		for (const [name, script, markup] of idioms) {
			writeFileSync(
				join(dir, `${name}.svelte`),
				`<script lang="ts">\n\t${script}\n</script>\n\n<p>${markup}</p>\n`
			);
		}

		const output =
			run('npx', ['eslint', '--no-ignore', dir]) +
			run('npx', ['svelte-check', '--tsconfig', './tsconfig.json', '--tsgo', '--output', 'human']);

		for (const [name] of idioms) {
			expect(output, `${name} was accepted`).toContain(`${name}.svelte`);
		}
	}
);
