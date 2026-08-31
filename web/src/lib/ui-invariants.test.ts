import { readdirSync, readFileSync, statSync } from 'node:fs';
import { extname, join } from 'node:path';
import { describe, expect, it } from 'vitest';

/**
 * The SPA's own source, minus the shadcn-svelte components — those are copied in from
 * upstream (ADR-002) and are not where this project's invariants can be broken.
 */
function sources(): Array<[path: string, text: string]> {
	const out: Array<[string, string]> = [];
	const walk = (dir: string) => {
		for (const entry of readdirSync(dir)) {
			const path = join(dir, entry);
			if (path.includes(join('components', 'ui'))) continue;
			if (statSync(path).isDirectory()) {
				walk(path);
				continue;
			}
			if (!['.svelte', '.ts'].includes(extname(path))) continue;
			if (path.endsWith('.test.ts')) continue;
			out.push([path, readFileSync(path, 'utf8')]);
		}
	};
	walk('src');
	return out;
}

describe('F3 — the UI renders from allowed_actions, never from a role name', () => {
	// `↯` Client-side hiding is cosmetic; the server checks every request regardless
	// (`09 §4.2`). The reason this is still an invariant is that a role branch *drifts*: a
	// grant model gains a capability, the server honours it, and the button stays hidden for
	// everyone whose role does not match — a permission that exists and cannot be used.
	it('no component compares a role to a literal', () => {
		const offenders: string[] = [];
		for (const [path, text] of sources()) {
			for (const pattern of [/role\s*===?\s*['"]/, /['"](admin|member)['"]\s*===?\s*/]) {
				if (pattern.test(text)) offenders.push(path);
			}
		}
		expect(offenders, 'render from allowed_actions instead (F3, `09 §4.2`)').toEqual([]);
	});

	it('the create button is gated on a capability the server sends', () => {
		const list = readFileSync(join('src', 'routes', '+page.svelte'), 'utf8');
		expect(list).toContain('session.allowedGlobally().includes(actions.create)');
	});
});

// `↯` Q25. M0's crossplay capture logged `New session server "<name>" that has join code ,`
// — the field is empty. A join code is exactly what a friend group wants a panel to show,
// which is why the temptation to promise one is worth a test rather than a comment. It may
// be assigned later, or need a setting, or only exist for a community-hosted session; until
// someone finds out, the panel says nothing about it.
it('Q25 — nothing in the SPA promises a crossplay join code', () => {
	const offenders: string[] = [];
	for (const [path, text] of sources()) {
		if (/join\s*code/i.test(text) && !path.endsWith('ui-invariants.test.ts')) offenders.push(path);
	}
	expect(offenders, 'Q25 is open: do not promise a join code until it has been found').toEqual([]);
});

// F2 / `02 §2.1`: if the frontend needs to know what a preset is, the backend failed to send
// it. The measured vocabulary of `03 §1.3` is served by GET /game/options, so no list of it
// should exist here.
it('F2 — the measured game vocabulary is not hardcoded in the SPA', () => {
	const offenders: string[] = [];
	for (const [path, text] of sources()) {
		// Any three of the eight measured preset names together is a copied list, not a
		// coincidence.
		const names = ['casual', 'hardcore', 'immersive', 'hammer'].filter((n) =>
			new RegExp(`['"]${n}['"]`).test(text)
		);
		if (names.length >= 2) offenders.push(`${path} (${names.join(', ')})`);
	}
	expect(offenders, 'the preset list comes from GET /game/options (F2)').toEqual([]);
});

// `↯` F5: every destructive action names the thing being destroyed. On this panel the thing
// behind a reflex-dismissed "Are you sure?" is somebody's world, so the confirmation makes
// the operator type the name back.
it('F5 — deleting an instance goes through the confirmation that names it', () => {
	const list = readFileSync(join('src', 'routes', '+page.svelte'), 'utf8');
	expect(list).toContain('DestructiveConfirm');
	expect(list, 'the dialog must be given the name to require back').toMatch(
		/DestructiveConfirm[\s\S]{0,400}name=\{/
	);
	expect(list, 'nothing may call remove() outside the confirmation').not.toMatch(
		/onclick=\{[^}]*instances\.remove/
	);

	const dialog = readFileSync(
		join('src', 'lib', 'components', 'destructive-confirm.svelte'),
		'utf8'
	);
	expect(dialog, 'confirm must be gated on the typed name matching').toContain(
		'disabled={!matches}'
	);
});

// `11 §2.4`: one request, one response, all the problems — rendered per field, from the
// field codes, and not as a single blob at the top of the form.
it('the create wizard renders per-field validation', () => {
	const wizard = readFileSync(join('src', 'routes', 'instances', 'new', '+page.svelte'), 'utf8');
	for (const field of ['name', 'server_name', 'world_name', 'password']) {
		expect(wizard, `${field} has no per-field message`).toContain(`problem('${field}')`);
	}
	// The helper reads the server's field codes rather than matching on prose.
	expect(wizard).toContain('apiError?.field(field)');
});

// F4: optimistic UI is forbidden for anything touching world data — the wizard shows the
// job the daemon reports, including the long flat stretch while a full ~1 GB copy runs.
it('provisioning shows the real job rather than a guess', () => {
	const wizard = readFileSync(join('src', 'routes', 'instances', 'new', '+page.svelte'), 'utf8');
	expect(wizard).toContain('JobProgress');
	const progress = readFileSync(join('src', 'lib', 'components', 'job-progress.svelte'), 'utf8');
	expect(progress).toContain('watchJob');
	expect(progress, 'the bar must be the reported value').toContain('value={job.progress}');
});
