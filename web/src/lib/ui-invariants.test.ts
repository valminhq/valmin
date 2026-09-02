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

// `↯` E3, `07 §5`, `03 §7`. The command channel resolves to `none` on this build — `strace`
// showed zero reads on fd 0 — so the console is output only. The input is present and
// disabled with the reason attached, because "where do I type" is the first question a
// console raises. `02 §4.4`: nothing may imply a shutdown warning can reach players.
it('E3 — the console input is disabled and says why', () => {
	const view = readFileSync(join('src', 'lib', 'components', 'console-view.svelte'), 'utf8');
	expect(view, 'the input must exist so its absence is not read as a bug').toMatch(/<input[^>]/);
	// `disabled` as its own attribute, not the `disabled:` Tailwind variant in the class —
	// which is what this assertion originally matched, so it passed with the attribute gone.
	expect(view, 'and it must be disabled').toMatch(/<input[\s\S]{0,300}?\sdisabled[\s>]/);
	expect(view, 'with the reason rendered, not only commented').toContain('console-input-reason');
});

// `↯` E7, Q7. Join/leave patterns were deliberately deferred past 1.0 as the most
// version-sensitive thing on the list, so `players` is null in every sample. Rendering it as
// 0 would be a number an operator could act on, invented by the panel.
it('E7 — players renders as unknown, never as a count', () => {
	const detail = readFileSync(join('src', 'routes', 'instances', '[id]', '+page.svelte'), 'utf8');
	expect(detail).toMatch(/Players[\s\S]{0,200}unknown/);
	expect(detail, 'nothing may read a player count out of a sample').not.toMatch(/\.players\b/);
});

// `↯` E7 again, and `14 §4.3` corrects its own justification: the cache term measured 0.1%
// of total on a freshly-started container, and nobody has checked it on a server up for
// days. Show memory; do not alarm on it until someone has.
//
// `↯` This catches the obvious implementation and says so rather than pretending otherwise.
// A regex cannot recognise "an alarm" in general — the first draft matched a threshold
// comparison and a `?? 0` in the middle of the expression walked straight past it. What it
// does catch is the name anyone would reach for first, which is where this would actually
// appear.
it('E7 — there is no memory alarm threshold', () => {
	const offenders: string[] = [];
	for (const [path, text] of sources()) {
		if (/mem(ory)?[A-Za-z_]*(alarm|threshold|warn|critical|danger)/i.test(text)) {
			offenders.push(path);
		}
	}
	expect(offenders, 'no memory threshold until one has been measured (E7, `14 §4.3`)').toEqual([]);
});

// `↯` ADR-039, `14 §4.2`. Lines go missing two ways — the hub dropped them because this
// browser fell behind (`gap`), or the server's ring rotated past them (a jump in `seq`) —
// and both render as a visible break. A console that quietly closes a hole is worse than one
// that admits it, because a reader draws conclusions from adjacency.
it('a gap is a visible break, and a reset clears rather than splices', () => {
	const buffer = readFileSync(join('src', 'lib', 'state', 'console.svelte.ts'), 'utf8');
	expect(buffer, "a 'gap' message must produce a break row").toMatch(
		/case 'gap':[\s\S]{0,400}kind: 'break'/
	);
	expect(buffer, "'stream.reset' must clear the view").toMatch(
		/case 'stream\.reset':[\s\S]{0,400}this\.reset\(\)/
	);

	const view = readFileSync(join('src', 'lib', 'components', 'console-view.svelte'), 'utf8');
	expect(view, 'the break must be rendered, not swallowed').toContain("row.kind === 'break'");
});

// `↯` G8, `14 §4.2`. The pinned startup segment is the first thing the server's ring drops
// and the only thing that explains a boot that failed, so it needs a control of its own —
// not a scrollbar and a hope.
it('G8 — the pinned startup segment is reachable from the UI', () => {
	const view = readFileSync(join('src', 'lib', 'components', 'console-view.svelte'), 'utf8');
	expect(view).toContain('Server start');
	expect(view, 'the jump must land on the first row').toMatch(/scrollToIndex\(0/);
});

// `↯` F1 / ADR-100. `@tanstack/svelte-virtual` hands back a Svelte 4 `Readable`, so using it
// forces `$store` autosubscription in every consuming component — and its `derived` returns
// the same mutated instance every time, so a rune bridge over it silently stops
// re-rendering. virtual-core is the same library one layer down. This test exists because
// the plan and `06 §4` both still name the adapter, and re-adding it would look like a fix.
it('ADR-100 — the Svelte 4 virtualizer adapter is not a dependency', () => {
	const pkg = JSON.parse(readFileSync('package.json', 'utf8')) as {
		dependencies?: Record<string, string>;
		devDependencies?: Record<string, string>;
	};
	const all = { ...pkg.dependencies, ...pkg.devDependencies };
	expect(
		Object.keys(all),
		'use @tanstack/virtual-core with the runes wrapper in src/lib/virtual.svelte.ts'
	).not.toContain('@tanstack/svelte-virtual');
});

// `↯` `06 §4` picks one icon set — `@lucide/svelte`, scoped — and says why in the same
// breath: "using both ships two icon libraries". That is not hypothetical. shadcn-svelte's
// `nova` style writes `@hugeicons/*` imports into the components it generates, and five of
// them arrived that way at WP-22 — four chevrons and a tick, pulling a whole second icon
// runtime behind them. `↯` The payoff is **two fewer dependencies and one icon set, not
// bytes**: swapping them saved 1,941 bytes of client JS out of ~905 KB, because lucide's
// icons replaced hugeicons' roughly one for one. The components are ours to maintain
// (ADR-002), so they were swapped anyway — and this test exists because the next
// `shadcn-svelte add` will reintroduce them, silently.
it('one icon library, not two', () => {
	const offenders: string[] = [];
	const walk = (dir: string) => {
		for (const entry of readdirSync(dir)) {
			const path = join(dir, entry);
			if (statSync(path).isDirectory()) {
				walk(path);
				continue;
			}
			if (!['.svelte', '.ts'].includes(extname(path))) continue;
			if (path.endsWith('ui-invariants.test.ts')) continue;
			// Any icon import that is not the one `06 §4` chose.
			const text = readFileSync(path, 'utf8');
			for (const m of text.matchAll(/from\s+['"]([^'"]*icons?[^'"]*)['"]/g)) {
				if (!m[1].startsWith('@lucide/svelte')) offenders.push(`${path} → ${m[1]}`);
			}
		}
	};
	walk('src');
	expect(offenders, 'icons come from @lucide/svelte alone (`06 §4`)').toEqual([]);

	const pkg = JSON.parse(readFileSync('package.json', 'utf8')) as {
		dependencies?: Record<string, string>;
		devDependencies?: Record<string, string>;
	};
	const named = Object.keys({ ...pkg.dependencies, ...pkg.devDependencies });
	expect(named.filter((n) => /icon/i.test(n) && n !== '@lucide/svelte')).toEqual([]);
});

// The mod screen (WP-M2-11). `↯` ADR-103 stands and is restated rather than quietly
// inherited: these read the source, not a browser. A button wired to nothing passes them.
describe('the mod screen', () => {
	const modsPage = () =>
		readFileSync(join('src', 'routes', 'instances', '[id]', 'mods', '+page.svelte'), 'utf8');

	// `↯` F2 / `02 §2.1`, and the sharpest version of it in the codebase: the mod engine is
	// where the game knowledge lives (`02 §2.4`), so this is the screen most likely to grow a
	// copy of it. A placement rule, a loader's directory layout or an environment variable
	// name in the SPA is a second, weaker copy of a decision `03 §5`–`§6` already made — and
	// it would rot silently, the way the pack's own Doorstop variable names did (`03 §5.2`).
	// The panel's words reach the operator by being *sent*, not by being spelled here.
	it('F2 — no mod-loader vocabulary reaches the SPA', () => {
		const forbidden = [
			/bepinex/i,
			/doorstop/i,
			/chainloader/i,
			/manifest\.json/i,
			/plugins\//i,
			/patchers/i,
			/winhttp/i
		];
		const offenders: string[] = [];
		for (const [path, text] of sources()) {
			// Comments are stripped first. The invariant is about what the SPA *does* and what
			// it says out loud; a doc comment citing `03 §5.5` to explain why the console
			// virtualizes is a reference to a decision, not a copy of one.
			const body = text
				.replace(/<!--[\s\S]*?-->/g, '')
				.replace(/\/\*[\s\S]*?\*\//g, '')
				.replace(/(^|[^:])\/\/.*$/gm, '$1');
			for (const pattern of forbidden) {
				if (pattern.test(body)) offenders.push(`${path} → ${pattern}`);
			}
		}
		expect(offenders, 'placement and loader vocabulary stays server-side (F2)').toEqual([]);
	});

	// `↯` Q38. The daemon reports `loaded`, `not_seen` or null, and there is no measured
	// literal for a *failed* plugin (`03 §5.3`, ADR-110). A screen that invented the third
	// answer would report healthy mods as broken.
	it('Q38 — the SPA does not invent a failed load status', () => {
		const text = modsPage() + readFileSync(join('src', 'lib', 'api', 'mods.ts'), 'utf8');
		expect(text).toContain('not_seen');
		expect(text, 'no `failed` load status until one has been measured').not.toMatch(
			/load_status\s*===?\s*'failed'|'failed'\s*===?\s*\w*load/i
		);
	});

	// F3, on this screen specifically: what is shown comes from the action strings the
	// server sent, never from `role`. The scan above covers role literals everywhere; this
	// asserts the positive half.
	it('F3 — mod actions are gated on the capability, not a role', () => {
		expect(modsPage()).toContain('actions.modsManage');
	});

	// `↯` B11 / C19. The server refuses a mod change on a running instance independently —
	// client-side disabling is cosmetic. What it is *not* is optional: an operator who
	// cannot see why the button is dead goes looking for a bug instead of stopping the
	// server. The reason is rendered, and the button is bound to the same value that
	// produced it.
	it('B11 — mod actions are disabled with the reason visible while the server runs', () => {
		const text = modsPage();
		expect(text).toContain('This server is running. Stop it to install or remove mods.');
		expect(text, 'the reason must be rendered, not only computed').toMatch(
			/data-testid="mod-actions-blocked"[\s\S]{0,80}\{blocked\}/
		);
		expect(text, 'every mod action is bound to the same gate').toMatch(/disabled=\{!canAct/);
	});

	// `↯` `04 §3` puts resolve before install deliberately: the operator confirms the whole
	// closure before anything is downloaded or written. The install request must therefore be
	// unreachable from the row — only the dialog's own confirm may send it.
	it('the closure is confirmed before anything is installed', () => {
		const text = modsPage();
		expect(text, 'the dialog lists the resolved nodes').toMatch(/#each pending\.nodes/);
		expect(text, 'transitive packages are marked').toContain('node.transitive');
		expect(text, 'nothing may install straight from a row').not.toMatch(
			/onclick=\{[^}]*mods\.install/
		);
		expect(text).toMatch(/onclick=\{installConfirmed\}/);
	});

	// F5: a destructive action names what it destroys, and cannot be reached without the
	// confirmation that names it.
	it('F5 — uninstall names the mod and goes through a confirmation', () => {
		const text = modsPage();
		expect(text).toMatch(/Remove \{pending\.full_name\}\?/);
		expect(text, 'nothing may uninstall straight from a row').not.toMatch(
			/onclick=\{[^}]*mods\.uninstall/
		);
	});

	// F4: no optimistic UI on anything touching what is on disk. The list is re-read from the
	// daemon when the job it reported actually finishes.
	it('F4 — the mod list follows the job rather than predicting it', () => {
		const text = modsPage();
		expect(text).toContain('JobProgress');
		expect(text, 'the list is re-read on the job finishing').toMatch(
			/onfinish=\{[\s\S]{0,160}refresh\(\)/
		);
	});

	// `↯` An operator cannot be expected to search the catalogue for every mod they have
	// installed to find out whether it moved. The panel already knows — `latest_version` and
	// `is_deprecated` are synced — so the installed row is where it belongs. What it must not
	// do is *guess*: a package the index has never heard of, before the first sync or after
	// being pulled, gets no badge at all. "No newer version known" and "up to date" are
	// different claims and only one of them is supported by anything.
	it('an installed mod says when a newer version exists, and only when one is known', () => {
		const text = modsPage();
		expect(text, 'the answer comes from the synced catalogue row').toMatch(
			/catalogue\.get\(mod\.full_name\)/
		);
		expect(text, 'and from comparing it to what is installed').toMatch(
			/listing && listing\.latest_version !== mod\.version/
		);
		expect(text, 'the newer version is named, not merely hinted at').toMatch(
			/\{newer\.latest_version\} available/
		);
		expect(text, 'nothing is claimed when the catalogue has no row').toMatch(/\{#if newer\}/);
	});

	// `↯` Q37. `enabled` is a recorded label with no on-disk meaning — nothing reads it when
	// the server boots. A switch would be the silent-success shape `03 §5.2` warns about:
	// it flips, it saves, and the mod loads anyway. It stays out of the UI until Q37 says
	// what it should do.
	it('Q37 — there is no enabled toggle', () => {
		const text = modsPage();
		expect(text).not.toMatch(/Switch/);
		expect(text, 'nothing renders `enabled` as a control').not.toMatch(/mod\.enabled/);
	});
});
