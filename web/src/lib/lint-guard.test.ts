import { execFileSync } from 'node:child_process';
import {
	closeSync,
	mkdirSync,
	mkdtempSync,
	openSync,
	readFileSync,
	rmSync,
	writeFileSync
} from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterAll, expect, it } from 'vitest';

// Writes each Svelte 4 idiom into the project and asserts svelte-check rejects it. With
// `runes: true` in svelte.config.js the Svelte compiler rejects all three, and svelte-check,
// which `make lint` runs, reports it; eslint-plugin-svelte does not read that option.
//
// The probes live inside the project and the assertion requires the output to name them: a
// checker run on a tmpdir fails for its own reasons and names no file. Each probe uses the
// value it declares, so an unused-variable rule cannot satisfy the test instead.
//
// Deleting `compilerOptions: { runes: true }` from svelte.config.js makes this fail on
// `export let`.
const idioms: Array<[name: string, script: string, markup: string]> = [
	['export-let', 'export let name = "x";', '{name}'],
	['reactive-statement', 'let x = $state(1);\n\t$: doubled = x * 2;', '{doubled}'],
	['dollar-dollar-props', 'const p = $$props;', '{p}']
];

const dir = join('src', 'lib', '__lint_probe__');

afterAll(() => rmSync(dir, { recursive: true, force: true }));

// Node 26 can lose output from synchronous child-process pipes. File descriptors keep the
// expected checker failures observable to the assertion.
function run(command: string, args: string[]): string {
	const outputDir = mkdtempSync(join(tmpdir(), 'valmin-lint-'));
	const outputPath = join(outputDir, 'output');
	const output = openSync(outputPath, 'w');
	try {
		try {
			execFileSync(join('node_modules', '.bin', command), args, {
				cwd: process.cwd(),
				stdio: ['ignore', output, output]
			});
		} catch {
			// These probes are expected to make the checker fail.
		} finally {
			closeSync(output);
		}
		return readFileSync(outputPath, 'utf8');
	} finally {
		rmSync(outputDir, { recursive: true, force: true });
	}
}

// Spawning a checker is slow and worth it once: this is the only thing standing between a
// Svelte 4 idiom and a merge.
it('every Svelte 4 idiom is rejected by eslint or svelte-check', { timeout: 120_000 }, () => {
	mkdirSync(dir, { recursive: true });
	for (const [name, script, markup] of idioms) {
		writeFileSync(
			join(dir, `${name}.svelte`),
			`<script lang="ts">\n\t${script}\n</script>\n\n<p>${markup}</p>\n`
		);
	}

	const output =
		run('eslint', ['--no-ignore', dir]) +
		run('svelte-check', ['--tsconfig', './tsconfig.json', '--tsgo', '--output', 'human']);

	for (const [name] of idioms) {
		expect(output, `${name} was accepted`).toContain(`${name}.svelte`);
	}
});
