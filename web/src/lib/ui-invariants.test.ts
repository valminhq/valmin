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

// The one place this project asserts anything about the copied shadcn-svelte components
// (ADR-002), and it earns the exception. The switch shipped styled with `data-checked:` and
// `data-unchecked:` — the variants a newer upstream snippet uses — while the installed
// bits-ui emits `data-state="checked|unchecked"`. Neither variant ever matched, so the track
// got no background and the thumb never moved: a 32×18px transparent control on a white card,
// present in the DOM and clickable but invisible. Three toggles rendered that way in the
// create wizard, and crossplay was one of them, so an operator could not turn it on and had
// no way to tell the control was there at all.
//
// This is the failure shape `CLAUDE.md §9` names: it works, it logs nothing, it does nothing.
// A `svelte-check` pass and every source scan above are blind to it, because a class string
// that matches no variant is not an error anywhere — it is simply CSS that never applies.
//
// Scoped to the two variant names known to be wrong rather than generalised to "every
// `data-*` variant is one bits-ui emits". The general form needs the library's whole
// attribute vocabulary and is the upgrade if a second component is ever mis-copied.
describe('the copied components are styled for the bits-ui that is installed', () => {
	function uiComponents(): Array<[path: string, text: string]> {
		const out: Array<[string, string]> = [];
		const walk = (dir: string) => {
			for (const entry of readdirSync(dir)) {
				const path = join(dir, entry);
				if (statSync(path).isDirectory()) walk(path);
				else if (extname(path) === '.svelte') out.push([path, readFileSync(path, 'utf8')]);
			}
		};
		walk(join('src', 'lib', 'components', 'ui'));
		return out;
	}

	it('no component styles a state bits-ui does not emit', () => {
		const offenders = uiComponents()
			.filter(([, text]) => /data-(un)?checked:/.test(text))
			.map(([path]) => path);
		expect(offenders, 'bits-ui emits data-state; use data-[state=checked]:').toEqual([]);
	});

	it('the switch track is coloured in both states', () => {
		const [, text] = uiComponents().find(([path]) => path.endsWith('switch.svelte')) ?? ['', ''];
		expect(text, 'the checked track').toMatch(/data-\[state=checked\]:bg-/);
		expect(text, 'the unchecked track — without it the control is invisible').toMatch(
			/data-\[state=unchecked\]:bg-/
		);
	});
});

describe('F3 — the UI renders from allowed_actions, never from a role name', () => {
	// Client-side hiding is cosmetic; the server checks every request regardless
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

// Q25, closed: the code is blank in the registration line and carried by the session line the
// server logs once it is active. What replaced the ban on promising one is narrower — the
// panel shows a code the daemon read from a log, and shows nothing at all when it sent null.
// A placeholder here is worse than a blank, because it is a code someone will try to use.
describe('the crossplay join code', () => {
	it('is rendered only where the daemon sent one', () => {
		for (const path of [
			join('src', 'routes', '+page.svelte'),
			join('src', 'routes', 'instances', '[id]', '+page.svelte')
		]) {
			expect(readFileSync(path, 'utf8'), `${path} must gate it on the field`).toMatch(
				/\{#if [\w.]*\.crossplay_join_code\}/
			);
		}
	});

	it('holds no code of its own', () => {
		const text = readFileSync(join('src', 'lib', 'components', 'join-code.svelte'), 'utf8');
		expect(text).toMatch(/\{code\}/);
		expect(text, 'a sample code here is a code an operator will try').not.toMatch(/\d{4,}/);
	});
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

// F5: every destructive action names the thing being destroyed. On this panel the thing
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

describe('the orphan adoption screen', () => {
	const api = () => readFileSync(join('src', 'lib', 'api', 'instances.ts'), 'utf8');
	const dashboard = () => readFileSync(join('src', 'routes', '+page.svelte'), 'utf8');
	const adoption = () =>
		readFileSync(
			join('src', 'routes', 'instances', 'adopt', '[container_id]', '+page.svelte'),
			'utf8'
		);

	it('F3 — discovery and adoption are gated on the capability the daemon sends', () => {
		expect(dashboard()).toMatch(
			/canAdopt = \$derived\(session\.allowedGlobally\(\)\.includes\(actions\.adopt\)\)/
		);
		expect(dashboard(), 'orphan discovery must not run without the capability').toMatch(
			/if \(!canAdopt\) \{[\s\S]{0,150}return;[\s\S]{0,150}orphans\(\)/
		);
		expect(adoption()).toMatch(
			/canAdopt = \$derived\(session\.allowedGlobally\(\)\.includes\(actions\.adopt\)\)/
		);
		expect(adoption(), 'the preview must not be fetched without the capability').toMatch(
			/if \(!canAdopt\) return;[\s\S]{0,150}adoption\.preview\(containerID\)/
		);
		for (const text of [dashboard(), adoption()]) {
			expect(text, 'the capability gate must not be replaced by a role check').not.toMatch(
				/role\s*===?\s*['"]|['"](?:admin|member)['"]\s*===?/
			);
		}
	});

	it('posts every mutable launch field the daemon requires', () => {
		const apiText = api();
		const page = adoption();
		const request = apiText.match(/export interface AdoptInstance \{([\s\S]*?)\n\}/)?.[1] ?? '';
		const body = page.match(/const body: AdoptInstance = \{([\s\S]*?)\n\t\t\};/)?.[1] ?? '';
		const fields = [
			'name',
			'server_name',
			'world_name',
			'password',
			'public',
			'crossplay',
			'preset',
			'modifiers',
			'extra_args',
			'mem_limit_mb',
			'cpu_limit'
		];
		for (const field of fields) {
			expect(request, `${field} must be required by the API type`).toMatch(
				new RegExp(`\\b${field}:`)
			);
			expect(body, `${field} must be present in the adoption request`).toMatch(
				new RegExp(`\\b${field}(?:\\s*:|\\s*[,\\n])`)
			);
		}
		expect(apiText).toMatch(
			/adopt: \(containerID: string, body: AdoptInstance\) =>[\s\S]{0,150}api\.post<Job>\(`\/orphans\/\$\{encodeURIComponent\(containerID\)\}`, body\)/
		);
	});

	it('F4 — follows the daemon job and refreshes only after success', () => {
		const text = adoption();
		const finished =
			text.match(/async function finished\(result: Job\) \{[\s\S]*?\n\t\}/)?.[0] ?? '';
		expect(text).toMatch(/<JobProgress jobId=\{job\.job_id\} onfinish=\{finished\} \/>/);
		expect(finished).toMatch(
			/result\.status !== 'succeeded' \|\| !result\.instance_id\) return;[\s\S]{0,250}instanceList\.load\(\)[\s\S]{0,150}session\.refreshPermissions\(\)[\s\S]{0,250}goto\(resolve\('\/instances\/\[id\]', \{ id: result\.instance_id \}\)\)/
		);
		expect(
			text.replace(finished, ''),
			'refresh and navigation belong only to the success path'
		).not.toMatch(/instanceList\.load\(\)|session\.refreshPermissions\(\)|goto\(/);
	});

	it('promises visibly that the existing container and files are untouched', () => {
		const source = adoption();
		const markup = prose(source.slice(source.indexOf('</script>') + '</script>'.length));
		expect(markup).toMatch(/The existing container and its files stay in place/);
		expect(markup).toMatch(
			/never stops, recreates, copies, or changes this server during adoption/
		);
	});
});

describe('the clone screen', () => {
	const api = () => readFileSync(join('src', 'lib', 'api', 'instances.ts'), 'utf8');
	const detail = () =>
		readFileSync(join('src', 'routes', 'instances', '[id]', '+page.svelte'), 'utf8');
	const clone = () =>
		readFileSync(join('src', 'routes', 'instances', '[id]', 'clone', '+page.svelte'), 'utf8');

	it('uses the clone action and endpoint the daemon owns', () => {
		expect(api()).toMatch(/clone: 'instance\.clone'/);
		expect(api()).toMatch(
			/clone: \(id: string, name: string\) => api\.post<Job>\(`\/instances\/\$\{id\}\/clone`, \{ name \}\)/
		);
	});

	it('F3 — the stopped-only detail link is gated on allowed_actions', () => {
		const text = detail();
		expect(text).toMatch(
			/\{#if allowed\.includes\(actions\.clone\)\}[\s\S]{0,500}disabled=\{inst\.state !== 'stopped'\}[\s\S]{0,500}\/instances\/\[id\]\/clone/
		);
	});

	it('states what is copied, what is fresh, and what remains separate', () => {
		const text = prose(clone());
		expect(text).toMatch(
			/world, installed game build, mods, settings files, launch settings, and game password/
		);
		expect(text).toMatch(/own ports, identity, data directories, and stopped container/);
		expect(text).toMatch(/Users, access grants, and the source backup catalogue are not copied/);
		expect(text).toMatch(/Changes to either server after cloning do not affect the other/);
	});

	it('refuses a running source visibly and follows the daemon job', () => {
		const text = clone();
		expect(text).toMatch(/ready = \$derived\([\s\S]{0,200}source\?\.state === 'stopped'/);
		expect(text).toContain('Cloning never disconnects players or stops the source server for you.');
		expect(text).toMatch(/<JobProgress jobId=\{job\.job_id\} onfinish=\{finished\}/);
		expect(text).toMatch(
			/finished\(result: Job\) \{[\s\S]{0,200}result\.status !== 'succeeded' \|\| !result\.instance_id[\s\S]{0,300}result\.instance_id/
		);
	});
});

// E3, `07 §5`, `03 §7`. The command channel resolves to `none` on this build — `strace`
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

// E7. The daemon sends null whenever it cannot tell — no reader, a stream that restarted, a
// peer that timed out without the server printing a new count. Rendering that as 0 would be a
// number an operator could act on, invented by the panel, so every reader of the field pairs
// it with a fallback that is not a number.
it('E7 — a null player count renders as unknown, never as 0', () => {
	const detail = readFileSync(join('src', 'routes', 'instances', '[id]', '+page.svelte'), 'utf8');
	expect(detail).toMatch(/Players[\s\S]{0,300}unknown/);
	expect(detail, 'the live count must fall back to a word, not a number').toMatch(
		/\.players \?\? 'unknown'/
	);
	expect(detail, 'and never to zero').not.toMatch(/\.players \?\? 0/);

	const history = readFileSync(join('src', 'lib', 'components', 'player-history.svelte'), 'utf8');
	expect(history, 'an observation gap is a gap, not an empty server').toContain('not observed');
	expect(history, 'and is never coerced to a count').not.toMatch(/players \?\? 0/);
});

// E7 again, and `14 §4.3` corrects its own justification: the cache term measured 0.1%
// of total on a freshly-started container, and nobody has checked it on a server up for
// days. Show memory; do not alarm on it until someone has.
//
// This catches the obvious implementation and says so rather than pretending otherwise.
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

// ADR-039, `14 §4.2`. Lines go missing two ways — the hub dropped them because this
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

// G8, `14 §4.2`. The pinned startup segment is the first thing the server's ring drops
// and the only thing that explains a boot that failed, so it needs a control of its own —
// not a scrollbar and a hope.
it('G8 — the pinned startup segment is reachable from the UI', () => {
	const view = readFileSync(join('src', 'lib', 'components', 'console-view.svelte'), 'utf8');
	expect(view).toContain('Server start');
	expect(view, 'the jump must land on the first row').toMatch(/scrollToIndex\(0/);
});

// F1 / ADR-100. `@tanstack/svelte-virtual` hands back a Svelte 4 `Readable`, so using it
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

// `06 §4` picks one icon set — `@lucide/svelte`, scoped — and says why in the same
// breath: "using both ships two icon libraries". That is not hypothetical. shadcn-svelte's
// `nova` style writes `@hugeicons/*` imports into the components it generates, and five
// arrived that way — four chevrons and a tick, pulling a whole second icon runtime behind
// them. The payoff is two fewer dependencies and one icon set, not bytes: swapping them
// saved 1,941 bytes of client JS out of ~905 KB, because lucide's
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

// The mod screen. ADR-103 stands and is restated rather than quietly inherited: these read
// the source, not a browser. A button wired to nothing passes them.
describe('the mod screen', () => {
	const modsPage = () =>
		readFileSync(join('src', 'routes', 'instances', '[id]', 'mods', '+page.svelte'), 'utf8');

	// F2 / `02 §2.1`, and the sharpest version of it in the codebase: the mod engine is
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

	// Q38. The daemon reports `loaded`, `not_seen` or null, and there is no measured
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

	// B11 / C19. The server refuses a mod change on a running instance independently —
	// client-side disabling is cosmetic. What it is *not* is optional: an operator who
	// cannot see why the button is dead goes looking for a bug instead of stopping the
	// server. The reason is rendered, and the button is bound to the same value that
	// produced it.
	it('B11 — mod actions are disabled with the reason visible while the server runs', () => {
		const text = modsPage();
		expect(text).toContain('This server is running. Stop it to install, remove, or label mods.');
		expect(text, 'the reason must be rendered, not only computed').toMatch(
			/data-testid="mod-actions-blocked"[\s\S]{0,80}\{blocked\}/
		);
		expect(text, 'every mod action is bound to the same gate').toMatch(/disabled=\{!canAct/);
	});

	// `04 §3` puts resolve before install deliberately: the operator confirms the whole
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

	// An operator cannot be expected to search the catalogue for every mod they have
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
		expect(text, 'the installed row offers the update beside that version').toMatch(
			/\{newer\.latest_version\} available[\s\S]{0,500}onclick=\{\(\) => askToInstall\(newer\)\}/
		);
		expect(text, 'the update action is visually distinct').toContain('<ArrowUpCircle />');
		expect(text, 'the confirmation names an update as an update').toMatch(
			/\{updating \? 'Update' : 'Install'\}/
		);
		expect(text, 'nothing is claimed when the catalogue has no row').toMatch(/\{#if newer\}/);
	});

	// Q37. `enabled` is a recorded label with no on-disk meaning — nothing reads it when
	// the server boots. A switch would be the silent-success shape `03 §5.2` warns about:
	// it flips, it saves, and the mod loads anyway. It stays out of the UI until Q37 says
	// what it should do.
	it('Q37 — there is no enabled toggle', () => {
		const text = modsPage();
		expect(text).not.toMatch(/Switch/);
		expect(text, 'nothing renders `enabled` as a control').not.toMatch(/mod\.enabled/);
	});

	it('side tags use the daemon vocabulary and remain visible when unknown', () => {
		const page = modsPage();
		const api = readFileSync(join('src', 'lib', 'api', 'mods.ts'), 'utf8');
		for (const side of ['server_only', 'client_required', 'client_optional', 'unknown']) {
			expect(api).toContain(`'${side}'`);
		}
		expect(api).toMatch(/api\.patch<InstalledMod>/);
		expect(page).toContain('sideLabel(mod.side)');
		expect(page, 'unknown is a useful recorded state, not an absent badge').not.toContain(
			"mod.side !== 'unknown'"
		);
	});

	it('side tags are re-read and never stop a running server implicitly', () => {
		const text = modsPage();
		expect(text).toMatch(/await mods\.setSide\(id, mod\.full_name, side\);\s*await refresh\(\)/);
		expect(text, 'a racing stopped-state refusal reaches the page problem').toMatch(
			/catch \(err\) \{\s*failure = err;/
		);
		expect(text).not.toContain('instances.stop');
	});
});

// Q42. Mods are chosen in the create wizard because the wizard can start the server
// itself and the world is written on that first boot — so the ordering the daemon
// implements (provision, install, start) has to be the ordering the screen promises. These
// are source scans, and ADR-103's caveat stands: they prove the wiring exists, not that a
// human can complete the flow.
describe('the create wizard can install mods', () => {
	const wizard = () =>
		readFileSync(join('src', 'routes', 'instances', 'new', '+page.svelte'), 'utf8');
	const picker = () => readFileSync(join('src', 'lib', 'components', 'mod-picker.svelte'), 'utf8');

	it('the wizard offers a picker and sends what it collects', () => {
		const text = wizard();
		expect(text, 'the picker is on the form').toMatch(/<ModPicker\b/);
		expect(text, 'and its choices reach the request body').toMatch(/body\.mods\s*=/);
		expect(text, 'each entry carries the version, not just the name').toMatch(
			/full_name:\s*m\.full_name,\s*version:\s*m\.latest_version/
		);
	});

	it('the wizard tells the operator when the mods go on', () => {
		// Whitespace-normalised: the copy wraps, and where prettier breaks the line is not
		// something this test has an opinion about.
		const prose = wizard().replaceAll(/\s+/g, ' ');
		expect(
			prose,
			'the reason mods are here at all is the ordering; say it rather than implying it'
		).toMatch(/before the server first starts/);
	});

	// F2, and this component is the likeliest place to break it: it is a mod screen, so the
	// pull toward "just check if it is BepInEx" is strongest here.
	it('the picker holds no game or loader logic', () => {
		const code = picker().replaceAll(/\/\*[\s\S]*?\*\/|(^|[^:])\/\/.*$/gm, '$1');
		for (const word of ['BepInEx', 'Doorstop', 'Valheim', 'plugins/', 'winhttp']) {
			expect(code, `${word} is game vocabulary and belongs to the daemon`).not.toContain(word);
		}
	});

	// The picker is used where no instance exists yet, so anything it did with an instance id
	// would be wrong by construction.
	it('the picker asks for nothing instance-shaped', () => {
		expect(
			picker(),
			'no instance id reaches a component used before the instance exists'
		).not.toMatch(/instanceId|instance_id|\/instances\//);
	});
});

// The config editor. ADR-103 stands: these read the source, not a browser.
describe('the config editor', () => {
	const listPage = () =>
		readFileSync(join('src', 'routes', 'instances', '[id]', 'configs', '+page.svelte'), 'utf8');
	const filePage = () =>
		readFileSync(
			join('src', 'routes', 'instances', '[id]', 'configs', '[file]', '+page.svelte'),
			'utf8'
		);
	const control = () =>
		readFileSync(join('src', 'lib', 'components', 'config-setting.svelte'), 'utf8');
	const rawEditor = () =>
		readFileSync(join('src', 'lib', 'components', 'config-raw.svelte'), 'utf8');
	const client = () => readFileSync(join('src', 'lib', 'api', 'client.ts'), 'utf8');

	// F2, and this is the screen where breaking it is most tempting: a `.cfg` declares its
	// own types, and a form is exactly where somebody would branch on one to pick a control.
	// `03 §9`'s mapping table lives on the server and reaches the SPA as `widget`. Quoted
	// literals only — `String` and `Boolean` are also JavaScript builtins, and it is the
	// branch on a game type name that is forbidden, not the language.
	it('F2 — no `.cfg` type name appears in the SPA', () => {
		const names = ['Boolean', 'Int32', 'Single', 'Double', 'String', 'KeyboardShortcut', 'Color'];
		const offenders: string[] = [];
		for (const [path, text] of sources()) {
			for (const name of names) {
				if (new RegExp(`['"]${name}['"]`).test(text)) offenders.push(`${path} → ${name}`);
			}
		}
		expect(offenders, 'the daemon sends `widget`; the SPA never learns a type (F2)').toEqual([]);
	});

	it('the control branches on the widget the daemon chose', () => {
		const text = control();
		expect(text, 'every branch reads `widget`').toMatch(/setting\.widget === widgets\./);
		expect(text, 'nothing branches on the declared type').not.toMatch(/setting\.type ===/);
	});

	// F3: what an operator can do here comes from the action strings the server sent.
	it('F3 — editing is gated on the capability, not a role', () => {
		expect(filePage()).toContain('actions.configEdit');
		expect(listPage()).toContain('actions.configEdit');
	});

	// B11 / C19. The daemon refuses a config write on a running server independently
	// (ADR-012). An operator who cannot see why the form is dead goes looking for a bug
	// instead of stopping the server.
	it('B11 — the form is disabled with the reason visible while the server runs', () => {
		const text = filePage();
		expect(text).toContain('This server is running. Stop it to change its settings.');
		expect(text, 'the reason must be rendered, not only computed').toMatch(
			/data-testid="config-actions-blocked"[\s\S]{0,80}\{blocked\}/
		);
		expect(text, 'every control is bound to the same gate').toMatch(/disabled=\{!editable\}/);
	});

	// A hundred controls on one page is where a change gets made by accident, and the file
	// on the other side is one an operator has often hand-edited. The diff is confirmed
	// before anything is written, so nothing else may send the patch.
	it('nothing writes except the diff dialog’s confirm', () => {
		const text = filePage();
		expect(text, 'the dialog lists what changed, old and new').toMatch(/#each changed as field/);
		expect(text, 'no control may patch straight from the form').not.toMatch(
			/onclick=\{[^}]*configs\.patch|oninput=\{[^}]*configs\.patch/
		);
		expect(text).toMatch(/onclick=\{saveConfirmed\}/);
	});

	// F4: no optimistic UI on anything that reached the disk. What the file holds after a
	// save is read back from the daemon, never assumed from what was sent.
	it('F4 — the form re-reads after saving', () => {
		expect(filePage()).toMatch(/await configs\.patch\([\s\S]{0,120}await load\(id, file\)/);
	});

	// ADR-110: a `.cfg` is written by the plugin on its first launch, and saying so is
	// Valheim knowledge. The daemon composes the sentence; this screen renders it.
	it('the empty state is the daemon’s sentence, not one composed here', () => {
		const text = listPage();
		expect(text, 'the note is rendered as sent').toMatch(/\{note\}/);
		expect(text, 'nothing here explains why a file is missing').not.toMatch(/[Ss]tart the server/);
	});

	// G1, `11 §1.1`. The raw route replaces the file entirely, so a save that does not say
	// which version it started from silently takes the other writer's with it — and reports
	// success. Two co-admins editing one server is `01 §2`'s primary user, not a corner case.
	it('the raw PUT is unreachable without a held ETag', () => {
		expect(client(), 'the ETag is an argument, not an option').toMatch(
			/putText: \(path: string, text: string, etag: string\)/
		);
		expect(client(), 'and an empty one never reaches the network').toMatch(/if \(!etag\) throw/);
		expect(client(), 'it is sent as If-Match').toMatch(/'If-Match': etag/);
		expect(rawEditor(), 'the editor saves with the ETag it read').toMatch(
			/configs\.writeRaw\(id, file, text, etag\)/
		);
		expect(rawEditor(), 'and cannot save before it holds one').toMatch(/!etag/);
	});

	// F4, G1. A refused save is a decision, not a transient failure: the other version is
	// shown and the operator picks. An auto-merge invents a file neither of them wrote, and a
	// retry on the fresh ETag is the data loss the ETag existed to prevent.
	it('a stale raw save is never merged or retried on its own', () => {
		const text = rawEditor();
		expect(text, 'the refusal is recognised by its code').toContain("'stale_write'");
		expect(text, 'and answered by reading, not by writing again').toMatch(
			/stale_write'\)[\s\S]{0,160}configs\.readRaw/
		);
		expect(
			text.match(/configs\.writeRaw/g),
			'one save path in the whole component, so no failure handler can hold a second'
		).toHaveLength(1);
	});

	// Both comparison versions come from the daemon, which is the only thing that knows what
	// the file held: the SPA keeps no copy and no history of its own.
	it('the compared versions are read from the daemon and dated', () => {
		expect(filePage(), 'both copies come from the endpoint').toMatch(
			/configs\.copy\(targetID, targetFile, which\)/
		);
		expect(filePage(), 'a file with no copy yet is not an error').toMatch(
			/configs\.copy\(targetID, targetFile, which\)\.catch\(\(\) => null\)/
		);
		expect(filePage(), 'and each comparison says how old it is').toContain('captured_at');
		expect(control(), 'a setting is marked against the file, not against the pending edit').toMatch(
			/String\(reference\) !== String\(setting\.current\)/
		);
	});

	// F4 again, at the point it is most tempting to skip: restoring is a bulk edit, so it
	// fills the form and goes through the same confirmation as anything typed by hand.
	it('restoring a version writes nothing on its own', () => {
		const text = filePage();
		expect(text, 'the button fills the pending edits').toMatch(
			/function restoreAll\(\)[\s\S]{0,320}edits = \{ \.\.\.edits/
		);
		expect(text, 'and sends nothing itself').not.toMatch(
			/function restoreAll\(\)[\s\S]{0,320}configs\.patch/
		);
	});

	// The line view is the only one that can show a change the schema never modelled — a
	// comment, a reordered section, a line no setting owns. It diffs the editor rather than
	// the file, so an unsaved edit is shown as what it will be.
	it('the raw view diffs the kept version against what is in the editor', () => {
		expect(rawEditor()).toMatch(/hunks\(diffLines\(reference, text\)\)/);
		expect(rawEditor(), 'the compared bytes come from the daemon').toMatch(
			/configs\.readRawCopy\(id, file, which\)/
		);
	});

	// F3: the escape hatch bypasses every type and range the schema enforces, so it is its
	// own capability and the tab is absent without it.
	it('F3 — the raw tab is gated on its own action', () => {
		expect(filePage()).toContain('actions.configRaw');
		expect(filePage(), 'the tab exists only when the action does').toMatch(/\{#if canRaw\}/);
	});

	// `11 §2.4`: one request, one response, all the problems — rendered against the setting
	// that caused each, from the field codes rather than as a blob at the top of the form.
	it('a rejected save renders per setting', () => {
		expect(filePage(), 'the message comes from the server’s field codes').toContain(
			'apiError?.field(field)'
		);
		expect(filePage(), 'and is handed to the control it belongs to').toMatch(
			/problem=\{problem\(field\)\}/
		);
		expect(control(), 'which renders it').toMatch(/\{problem\}/);
	});
});

describe('the server settings screen', () => {
	const settings = () =>
		readFileSync(join('src', 'routes', 'instances', '[id]', 'settings', '+page.svelte'), 'utf8');
	const notice = () =>
		readFileSync(join('src', 'lib', 'components', 'restart-notice.svelte'), 'utf8');

	// F3. The daemon gates ordinary settings and resource limits separately (ADR-121). Both
	// gates come from `allowed_actions`, never a role.
	it('F3 — the controls are gated on the action the daemon sends', () => {
		const text = settings();
		expect(text).toContain('actions.settings');
		expect(text, 'ordinary settings use their capability').toMatch(
			/canEdit = \$derived\(allowed\.includes\(actions\.settings\)\)/
		);
		expect(text, 'limits use their separate never-grantable capability').toMatch(
			/canEditLimits = \$derived\(allowed\.includes\(actions\.limits\)\)/
		);
		expect(text, 'the limit controls use that gate').toMatch(
			/data-testid="limit-controls"[\s\S]*disabled=\{!canEditLimits\}/
		);
		expect(text, 'save checks the capability for every changed field').toContain(
			"field === 'mem_limit_mb' || field === 'cpu_limit' ? canEditLimits : canEdit"
		);
	});

	// Q48. `-world` names the save file basename, so renaming it moves the world's files
	// rather than writing a column — which is why it is absent from the PATCH. An operator who
	// cannot find a setting concludes the panel is broken; one who is told concludes it is
	// honest, so the field is shown, disabled, next to the reason.
	it('Q48 — world_name is shown, locked, and says why', () => {
		const text = settings();
		expect(text, 'the field is on screen').toContain('instance.world_name');
		expect(text, 'and not editable').toMatch(/id="world_name"[\s\S]{0,120}readonly/);
		expect(text, 'with the reason beside it').toMatch(/name of the save file on disk/);
		expect(text, 'nothing may send it — the daemon has no field for it').not.toMatch(
			/body\.world_name/
		);
	});

	// Q49 (E8). `03 §1.3.1` measured which preset names the parser accepts. What a changed
	// preset does to a world that already exists is a different question and is unmeasured, so
	// the screen must claim neither safety nor harm.
	it('Q49 — the preset and modifier fields say the effect is unmeasured', () => {
		expect(settings(), 'the untested claim is rendered, not only known').toMatch(
			/Nobody has measured what these do to a world that already exists/
		);
	});

	// B11 and ADR-118. The rebuild is what makes a launch edit real, so the notice states it —
	// and states it conditionally, because a mod install sets the same flag and triggers no
	// rebuild (ADR-107). The claim lives in one component so a future correction lands once;
	// the mod screen keeps its own wording, which names mods rather than launch settings.
	it('B11 — the restart notice is rendered, and names the rebuild', () => {
		expect(settings(), 'the settings screen shows it').toContain('<RestartNotice />');
		expect(notice(), 'and it says what the next start does').toMatch(/rebuilding its container/);
		const claims = sources().filter(([, text]) => /rebuilding its container/.test(text));
		expect(
			claims.map(([path]) => path),
			'the rebuild is claimed in exactly one place'
		).toEqual([join('src', 'lib', 'components', 'restart-notice.svelte')]);
	});

	// F4. The row on screen after a save is the one the daemon returned, never the one the
	// form sent — a rejected field, a trimmed name or a normalised modifier set would
	// otherwise be shown as saved.
	it('F4 — the form adopts the daemon’s row after a save', () => {
		const text = settings();
		expect(text).toMatch(/adopt\(await instances\.patch\(id, body\)\)/);
		expect(
			text.match(/instances\.patch/g),
			'one save path, so no second one can skip it'
		).toHaveLength(1);
		expect(text, 'and the baseline is only ever a row from the daemon').toMatch(
			/function adopt\(row: Instance\)/
		);
	});

	// F5. A new password locks every player out until someone tells them, and the panel
	// cannot. It is the one field on this screen that is not undone by typing it back.
	it('F5 — changing the password asks first and names what it affects', () => {
		const text = settings();
		expect(text, 'the confirmation is what the save button reaches').toMatch(
			/changed\.includes\('password'\)\) confirming = true/
		);
		expect(text, 'and it names the consequence').toMatch(/needs the new password/);
	});

	// PATCH semantics (`11 §1.1`): absent means unchanged. A form that sent every field would
	// re-encrypt an untouched password on every save and rewrite settings nobody edited.
	it('only the fields the operator touched are sent', () => {
		const text = settings();
		expect(text, 'the body is built from what changed').toMatch(
			/const body: PatchInstance = \{\};/
		);
		for (const field of [
			'server_name',
			'password',
			'public',
			'crossplay',
			'preset',
			'modifiers',
			'cpu_limit'
		]) {
			expect(text, `${field} is sent only when it changed`).toMatch(
				new RegExp(`changed\\.includes\\('${field}'\\)\\) body\\.${field} =`)
			);
		}
		expect(text, 'memory is sent only after its required numeric value is present').toMatch(
			/changed\.includes\('mem_limit_mb'\)[\s\S]{0,100}body\.mem_limit_mb =/
		);
	});

	it('invalid resource limits render beside their inputs', () => {
		const text = settings();
		for (const field of ['mem_limit_mb', 'cpu_limit']) {
			expect(text).toContain(`apiError?.field(field)`);
			expect(text).toMatch(new RegExp(`id="${field}-error"[\\s\\S]{0,120}problem\\('${field}'\\)`));
		}
		expect(text).toMatch(/builds the container on the next start/);
		expect(text).toMatch(/Saving does\s+not change the running container/);
	});
});

describe('the player-list screen', () => {
	const page = () =>
		readFileSync(join('src', 'routes', 'instances', '[id]', 'players', '+page.svelte'), 'utf8');
	const editor = () =>
		readFileSync(join('src', 'lib', 'components', 'player-list-editor.svelte'), 'utf8');
	const api = () => readFileSync(join('src', 'lib', 'api', 'players.ts'), 'utf8');

	it('renders all three daemon-owned lists behind players.manage', () => {
		expect(page()).toContain('actions.playersManage');
		for (const kind of ['admins', 'bans', 'permitted']) {
			expect(page()).toContain(`kind="${kind}"`);
		}
	});

	it('uses the ETag from the loaded representation for every replacement', () => {
		expect(api()).toMatch(/api\.getJSON<PlayerList>/);
		expect(api()).toMatch(/api\.putJSON<PlayerList>\(path\(instanceId, kind\), \{ ids \}, etag\)/);
		expect(editor()).toMatch(/playerLists\.put\(instanceId, kind, asIDs\(text\), match\)/);
	});

	it('keeps local edits visible while reviewing a stale-write conflict', () => {
		const text = editor();
		expect(text).toMatch(/err\.code === 'stale_write'/);
		expect(text).toContain('Your edits are still in the field above.');
		expect(text).toContain('Save my edits over it');
		expect(text).toContain('Use current version');
	});

	it('renders indexed API field errors beside the list input', () => {
		expect(editor()).toMatch(/field\.field\.startsWith\('ids\.'\)/);
		expect(editor()).toContain('Line {line}: {issue.message}');
	});

	it('does not invent a player-ID platform or reorder entries', () => {
		const text = editor();
		expect(text).toMatch(/return value\.split\('\\n'\)/);
		expect(text).not.toMatch(/\.sort\(/);
		expect(text).not.toMatch(/Steam_|PlayFab_/);
	});

	it('separates saving from the permission-gated ordinary restart job', () => {
		const text = page();
		expect(text).toContain('Saving a list does not restart the server.');
		expect(text).toContain('actions.restart');
		expect(text).toMatch(/instances\.restart\(id\)/);
	});
});

describe('the world import panel', () => {
	const panel = () => readFileSync(join('src', 'lib', 'components', 'world-import.svelte'), 'utf8');
	const filePicker = () =>
		readFileSync(join('src', 'lib', 'components', 'world-file-picker.svelte'), 'utf8');
	const settings = () =>
		readFileSync(join('src', 'routes', 'instances', '[id]', 'settings', '+page.svelte'), 'utf8');

	// Q41: the endpoint shipped at M1 and nothing called it, so the feature existed and was
	// unreachable. It is reachable from the settings screen.
	it('the panel is on the settings screen', () => {
		expect(settings()).toContain('<WorldImport {instance} />');
	});

	// F3. `world.import` is its own capability (`09 §3.2`), separate from every other action
	// on this screen, and the panel renders from `allowed_actions` rather than a role.
	it('F3 — the panel is gated on the action the daemon sends', () => {
		const text = panel();
		expect(text).toContain('actions.worldImport');
		expect(text, 'the gate is the capability, not the role').toMatch(
			/canImport = \$derived\(allowed\.includes\(actions\.worldImport\)\)/
		);
	});

	// F5. An import replaces the world this server loads. The pre-import archive makes it
	// recoverable, not undone, so the operator names what is being replaced before it happens.
	it('F5 — the import is unreachable without the confirmation that names the world', () => {
		const text = panel();
		expect(text).toContain('DestructiveConfirm');
		expect(text, 'the world being replaced is what must be typed back').toMatch(
			/name=\{instance\.world_name\}/
		);
		expect(text, 'nothing may import straight from the button').not.toMatch(
			/onclick=\{[^}]*instances\.importWorld/
		);
		expect(text, 'the confirmation is the only caller').toMatch(/onconfirm=\{start\}/);
	});

	// F4. The panel shows the job the daemon reports — including the pre-import archive
	// (`03 §4.1` rule 6), which on a large world is minutes of copying with nothing else to
	// see. A bar that ran ahead of it would be a lie exactly while the operator is deciding
	// whether something has hung.
	it('F4 — the panel follows the real job and predicts nothing', () => {
		const text = panel();
		expect(text).toContain('JobProgress');
		expect(text, 'the job id comes from the daemon’s 202').toMatch(/jobId = job\.job_id/);
		expect(text, 'nothing tracks an outcome of its own').not.toMatch(
			/\$state[^\n]*(success|done|imported)/i
		);
		expect(text, 'and finishing only releases the gate — it claims nothing').toMatch(
			/onfinish=\{\(\) => \(jobRunning = false\)\}/
		);
	});

	// `03 §4.1` is the daemon's, in full. A pair rule copied into the SPA is a second,
	// weaker copy that rots the day the daemon's changes — and an `accept` filter is that
	// copy in the one place it also hides files the daemon has an answer for.
	it('F2 — what counts as a world is not decided in the SPA', () => {
		expect(panel(), 'the panel picks files through the shared picker').toMatch(
			/<WorldFilePicker\b/
		);
		const text = filePicker();
		expect(text, 'the file picker offers no opinion').not.toMatch(/accept=/);
		expect(text, 'nothing inspects a filename locally').not.toMatch(
			/endsWith\(|\.name\.match|splitext/
		);
		// The refusal reaches the operator as the job's own error, which carries the rule
		// name the daemon refused under.
		const progress = readFileSync(join('src', 'lib', 'components', 'job-progress.svelte'), 'utf8');
		expect(progress, 'the daemon’s refusal is rendered verbatim').toContain('{job.error}');
	});

	// C19: no job on this panel stops a running server, and the daemon refuses an import into
	// one. The reason is on screen before the click rather than after it.
	it('the stopped-server requirement is stated, not just enforced', () => {
		expect(panel(), 'the reason is rendered').toMatch(/\{blocked \?\?/);
		expect(panel(), 'and gates the button').toMatch(/blocked === null/);
	});
});

describe('the create wizard can start from an existing world', () => {
	const wizard = () =>
		readFileSync(join('src', 'routes', 'instances', 'new', '+page.svelte'), 'utf8');

	// `03 §4.1`: offered here as well as post-hoc. One picker, so the pair rule and the
	// rolling-backup question are worded once rather than diverging between two screens.
	it('the wizard offers the same picker the instance screen uses', () => {
		const text = wizard();
		expect(text, 'the picker is on the form').toMatch(/<WorldFilePicker\b/);
		expect(text, 'and what it collects is what gets uploaded').toMatch(
			/instances\.importWorld\(\s*newInstanceId,\s*worldFiles,\s*allowBackupVariant/
		);
	});

	// F3. There is no instance to ask about at create time, so the gate reads the global list —
	// which `Allowed(u, "")` fills with every action for whoever may create a server at all.
	it('F3 — the import step is gated on the action the daemon sends', () => {
		const text = wizard();
		expect(text, 'the gate is the capability, not the role').toMatch(
			/canImport = \$derived\(session\.allowedGlobally\(\)\.includes\(actions\.worldImport\)\)/
		);
		expect(text, 'and the picker is behind it').toMatch(/\{#if canImport\}/);
		expect(text, 'a hidden picker cannot still put files on the request').toMatch(
			/worldFiles = \$derived\(canImport \?/
		);
	});

	// The path every existing operator takes. Nothing about creating a server without a world
	// may change shape, so the no-world branch is asserted rather than assumed.
	it('creating without a world is the same two steps it always was', () => {
		const text = wizard();
		expect(text, 'no world means the wizard keeps the start it was given').toMatch(
			/start_after_provision:\s*startAfter && worldFiles\.length === 0/
		);
		expect(text, 'and finishing goes straight to the list').toMatch(
			/return;\s*\}\s*await done\(\);/
		);
		expect(text, 'the import panel renders only once there is an import').toMatch(
			/\{#if importJob\}/
		);
	});

	// C19. The daemon refuses an import into anything but a stopped server, so a wizard that
	// chained a start would race its own import and lose.
	it('a world to import cancels the chained start rather than racing it', () => {
		const text = wizard();
		expect(text, 'the start is withheld while there is a world to bring').toMatch(
			/start_after_provision:\s*startAfter && worldFiles\.length === 0/
		);
		expect(text, 'and is submitted after the import instead').toMatch(
			/if \(startAfter && newInstanceId\) await instances\.start\(newInstanceId\)/
		);
		expect(prose(text), 'the operator is told why the order changed').toMatch(
			/A world is imported into a stopped server, so this one starts after the import/
		);
	});

	// There is no instance to import into until the provision job says there is, and its id
	// comes from the daemon's own 202 rather than from anything the wizard assembles.
	it('the import waits for the provision job and uses the id the daemon sent', () => {
		const text = wizard();
		expect(text, 'the id is read off the job').toMatch(
			/newInstanceId = \$derived\(job\?\.instance_id \?\? null\)/
		);
		expect(text, 'and nothing imports before the provision succeeded').toMatch(
			/finished\(finishedJob: Job\) \{\s*if \(finishedJob\.status !== 'succeeded'\) return;/
		);
	});

	// F4. Two jobs, both rendered from what the daemon reports. A failed import must not
	// redirect: the server was created and the world was not, and a redirect makes those
	// look like the same outcome.
	it('a failed import leaves the operator on the page that says so', () => {
		const text = wizard();
		expect(text, 'the import renders through the shared progress component').toMatch(
			/jobId=\{importJob\.job_id\} onfinish=\{imported\}/
		);
		expect(text, 'and only a success navigates away').toMatch(
			/imported\(finishedJob: Job\) \{[\s\S]*?if \(finishedJob\.status !== 'succeeded'\) return;/
		);
		expect(text, 'with a way to reach the server that does exist').toMatch(
			/href=\{resolve\('\/instances\/\[id\]', \{ id: newInstanceId \}\)\}/
		);
	});
});

/**
 * Copy read with runs of whitespace collapsed. Prettier reflows a sentence whenever the markup
 * around it changes, and a line break is not a change to what the screen says — an assertion
 * that fails on one is testing the formatter.
 */
const prose = (text: string) => text.replaceAll(/\s+/g, ' ');

describe('the backups panel', () => {
	const panel = () =>
		readFileSync(join('src', 'lib', 'components', 'backups-panel.svelte'), 'utf8');
	const route = () =>
		readFileSync(join('src', 'routes', 'instances', '[id]', 'backups', '+page.svelte'), 'utf8');
	const detail = () =>
		readFileSync(join('src', 'routes', 'instances', '[id]', '+page.svelte'), 'utf8');

	// The catalogue had rows and no way to see them. A panel nothing links to is the same
	// failure with more code in it.
	it('the panel is reachable from the server it belongs to', () => {
		expect(route()).toContain('<BackupsPanel');
		expect(detail(), 'the server page links to it').toContain('/instances/[id]/backups');
	});

	// F3. Four separate capabilities, each gating its own control, and the retention form is
	// `instance.settings` rather than any of them (ADR-121, ADR-126).
	it('F3 — every control is gated on the capability the daemon sends', () => {
		const text = panel();
		for (const gate of [
			/canList = \$derived\(allowed\.includes\(actions\.backupsList\)\)/,
			/canCreate = \$derived\(allowed\.includes\(actions\.backupsCreate\)\)/,
			/canDownload = \$derived\(allowed\.includes\(actions\.backupsDownload\)\)/,
			/canRestore = \$derived\(allowed\.includes\(actions\.backupsRestore\)\)/,
			/canSetPolicy = \$derived\(allowed\.includes\(actions\.settings\)\)/
		]) {
			expect(text, 'the gate is the capability, not the role').toMatch(gate);
		}
	});

	// `12 §3.2`. A quiesced backup stops the server, and that is the whole reason it can be
	// trusted. Copy that leaves it out turns a planned outage into a surprise one.
	it('the quiesced control says the server goes down', () => {
		const text = prose(panel());
		expect(text, 'the button names the stop').toMatch(/Stop and back up/);
		expect(text, 'and the copy says the server is offline for it').toMatch(
			/The server is stopped.*offline for the whole backup/
		);
	});

	// B12. A hot copy reads a world that is being written to. It ships because downtime is
	// sometimes not an option, and it must never read as the safe one.
	it('B12 — the hot copy is labelled best-effort and is not offered as the good archive', () => {
		const text = prose(panel());
		expect(text, 'the copy says best-effort').toMatch(/Best-effort/);
		expect(text, 'and says what it costs').toMatch(/half-written save/);
		expect(text, 'the trustworthy archive is the quiesced one').toMatch(
			/this is the archive worth restoring from/
		);
		expect(
			text,
			'a hot archive says so wherever it is listed, not only where it was taken'
		).toMatch(/!archive\.consistent/);
	});

	// F5. A restore replaces the world this server loads. The pre-restore archive makes it
	// recoverable, not undone.
	it('F5 — restoring and deleting both name what they act on', () => {
		const text = panel();
		expect(text, 'the world being replaced is typed back').toMatch(/name=\{instance\.world_name\}/);
		expect(text, 'an archive is named by its own file, since a world has several').toMatch(
			/name=\{deleting\?\.filename \?\? ''\}/
		);
		expect(text, 'nothing restores straight from the button').not.toMatch(
			/onclick=\{[^}]*backups\.restore/
		);
		expect(text, 'the confirmations are the only callers').toMatch(/onconfirm=\{restore\}/);
	});

	// F4. A quiesced backup is a stop, a whole-world copy and a start; a restore is the same
	// again. Both are the daemon's to report.
	it('F4 — the panel follows the real job and predicts nothing', () => {
		const text = panel();
		expect(text).toContain('JobProgress');
		expect(text, 'the job id comes from the daemon’s 202').toMatch(/jobId = job\.job_id/);
		expect(text, 'nothing tracks an outcome of its own').not.toMatch(
			/\$state[^\n]*(success|done|restored|archived)/i
		);
		expect(text, 'finishing re-reads the catalogue rather than assuming a row').toMatch(
			/onfinish=\{finished\}/
		);
	});

	// The two counts are separate because the classes are: one shared budget lets a burst of
	// cheap hot copies evict every quiesced archive.
	it('retention is two counts, not one', () => {
		const text = prose(panel());
		expect(text).toMatch(/backup_keep_cold: cold/);
		expect(text).toMatch(/backup_keep_hot: hot/);
		expect(text, 'and they are labelled by what they hold').toMatch(/Keep the last N full backups/);
		expect(text, 'the best-effort count says so').toMatch(/Keep the last N best-effort copies/);
	});

	// A retention setting whose effect is invisible until it deletes something is the wrong
	// shape for world data — and the judgement is the daemon's, over the whole catalogue,
	// because a count kept here would be a second copy of one policy that can only see a page.
	it('the list says which archives the next prune removes, and does not decide it here', () => {
		const text = prose(panel());
		expect(text, 'the marking is rendered').toMatch(/archive\.prunes_next/);
		expect(text).toMatch(/deleted next prune/);
		expect(text, 'nothing counts archives locally to decide it').not.toMatch(
			/(slice|filter)\([^)]*\)[^\n]*keep(Cold|Hot)/
		);
	});

	// It ships off by default and states its cost: a restart already pays for the stop, but it
	// then waits for the archive before the server comes back.
	it('the restart archive says what it costs', () => {
		const text = prose(panel());
		expect(text).toMatch(/backup_on_restart: onRestart/);
		expect(text, 'the wait is named').toMatch(/the restart waits for it/);
	});
});

describe('the schedules editor', () => {
	const editor = () =>
		readFileSync(join('src', 'lib', 'components', 'schedules-editor.svelte'), 'utf8');
	const api = () => readFileSync(join('src', 'lib', 'api', 'schedules.ts'), 'utf8');

	// ADR-132. A schedule is authorized by the action its tick would exercise, so the kinds on
	// offer are the ones this caller could run by hand.
	it('F3 — each kind is gated on the action its run would need', () => {
		expect(editor(), 'the list is filtered by capability').toMatch(
			/offered = \$derived\(scheduleKinds\.filter\(\(k\) => allowed\.includes\(k\.action\)\)\)/
		);
		expect(api(), 'and each kind carries the action it needs').toMatch(
			/kind: 'backup', action: 'backups\.create'/
		);
	});

	// The expression is the daemon's to validate: it answers an invalid one with the field and
	// its own help. A second parser here would drift from the one that actually runs.
	it('the cron expression is not parsed in the SPA', () => {
		const text = editor();
		expect(text, 'nothing splits or interprets the expression').not.toMatch(
			/cron\.(split|match|test)|parseCron|cronParse/
		);
	});

	// An operator who reads "04:00" and thinks in their own clock is the misunderstanding this
	// prevents, so the daemon sends the zone with the row and it is rendered.
	it('the timezone the schedule runs in is shown', () => {
		expect(editor()).toMatch(/times in \{s\.timezone\}/);
	});

	// F5: a schedule is something running unattended. Deleting it stops that silently
	// otherwise.
	it('F5 — deleting a schedule is confirmed', () => {
		expect(editor()).toContain('DestructiveConfirm');
		expect(editor(), 'nothing deletes straight from the button').not.toMatch(
			/onclick=\{[^}]*schedules\.remove/
		);
	});
});

describe('WP-M6-03 asynchronous component boundaries', () => {
	it('constructs the console virtualizer without tracking row count', () => {
		const text = readFileSync(join('src', 'lib', 'components', 'console-view.svelte'), 'utf8');
		expect(text).toContain("import { untrack } from 'svelte'");
		expect(text).toContain('untrack(() => buffer.rows.length)');
	});

	it('notifies completion once for each job id', () => {
		const text = readFileSync(join('src', 'lib', 'components', 'job-progress.svelte'), 'utf8');
		expect(text).toMatch(/let done = \$state\(false\)/);
		expect(text).toMatch(/if \(!done && isTerminal\(next\.status\)\)/);
	});

	it('tracks both config route parameters before loading', () => {
		const text = readFileSync(
			join('src', 'routes', 'instances', '[id]', 'configs', '[file]', '+page.svelte'),
			'utf8'
		);
		expect(text).toMatch(/void load\(id, file\)/);
		expect(text).toMatch(/async function load\(targetID: string, targetFile: string\)/);
	});

	it('rejects stale results from both catalogue search boxes', () => {
		for (const path of [
			join('src', 'lib', 'components', 'mod-picker.svelte'),
			join('src', 'routes', 'instances', '[id]', 'mods', '+page.svelte')
		]) {
			const text = readFileSync(path, 'utf8');
			expect(text, path).toMatch(/const request = \+\+searchRequest/);
			expect(text, path).toMatch(/if \(request !== searchRequest\) return/);
		}
	});

	it('does not submit an empty retention field', () => {
		const text = readFileSync(join('src', 'lib', 'components', 'backups-panel.svelte'), 'utf8');
		expect(text).toMatch(/const policyValid = \$derived/);
		expect(text).toContain('disabled={!policyValid || !policyChanged || savingPolicy}');
	});
});

describe('the update notice', () => {
	const notice = () =>
		readFileSync(join('src', 'lib', 'components', 'update-notice.svelte'), 'utf8');

	// `12 §2.5`: an available update is a property, not a state. The notice renders beside a
	// server that keeps running, and nothing changes until an operator says so.
	it('an available update is rendered as a property, never as a state', () => {
		const text = notice();
		expect(text, 'the flag is the daemon’s').toMatch(/status\?\.update_available === true/);
		expect(text, 'and it is not mixed into the state badge').not.toMatch(
			/state === 'update|StateBadge/
		);
	});

	// `03 §8`. A new build can break every mod on a server and the panel cannot check which, so
	// a modded server is told and left to its operator.
	it('a modded server carries notify-only copy', () => {
		const text = prose(notice());
		expect(text).toMatch(/\{#if instance\.modded\}/);
		expect(text, 'it says nothing updates it on its own').toMatch(
			/Nothing updates this server on its own/
		);
		expect(text, 'and that a schedule skips rather than runs it').toMatch(/records the skip/);
	});

	// F3, and `09 §3.3`: `instance.update` is never grantable, so the control renders from it
	// rather than from an admin check.
	it('F3 — the update control is gated on the capability, not a role', () => {
		const text = notice();
		expect(text).toMatch(/canUpdate = \$derived\(allowed\.includes\(actions\.gameUpdate\)\)/);
		expect(text, 'an admin can update before detection has run').toMatch(
			/\{#if newsworthy \|\| canUpdate\}/
		);
	});

	// A notice is for something to decide. A server already on the public build has nothing
	// outstanding, so it reports the build as a fact rather than standing in the notice
	// stack beside the ones that do need attention.
	it('being up to date is not rendered as a notice', () => {
		const text = notice();
		expect(text).toMatch(/const newsworthy = \$derived\(available \|\| unchecked\)/);
		expect(text, 'the alert is reserved for news').toMatch(/\{#if newsworthy\}\s*<Alert\.Root>/);
		expect(prose(text), 'and the quiet state names the build it is on').toMatch(
			/is the current public build/
		);
		expect(text, 'the action keeps one name through the flow').toContain(
			"available || unchecked ? 'Update' : 'Reinstall'"
		);
	});

	// The daemon requires a stopped server, and refuses a modded one without the confirmation.
	// Both are on screen before the click rather than after it.
	it('F5 — the update is confirmed, and the stopped requirement is stated', () => {
		const text = prose(notice());
		expect(text).toContain('DestructiveConfirm');
		expect(text, 'nothing updates straight from the button').not.toMatch(
			/onclick=\{[^}]*instances\.updateGame/
		);
		expect(text, 'the reason it cannot run is rendered').toMatch(/\{blocked \?\?/);
		expect(text, 'and B7 — the server is left stopped, not started').toMatch(
			/stays stopped afterwards/
		);
	});
});

describe('the server list update indicator', () => {
	it('shows an accessible icon badge only when an update is available', () => {
		const text = readFileSync(join('src', 'routes', '+page.svelte'), 'utf8');
		expect(text).toMatch(/instances\.updateStatus\(instance\.id\)/);
		expect(text).toMatch(/status\.update_available === true/);
		expect(text).toMatch(/\{#if updateAvailable\[instance\.id\]\}/);
		expect(text).toContain('ArrowUpCircle aria-hidden="true"');
		expect(text).toContain('Game update available');
	});
});

// ADR-166, closing Q26. Rotation moves derived keys and leaves the master key alone, so the
// one thing an operator reaching for it after a suspected leak must read is what it does not
// do — and they have to read it before they press the button, not in the result.
describe('the key rotation screen', () => {
	const page = () => readFileSync(join('src', 'routes', 'admin', 'keys', '+page.svelte'), 'utf8');

	it('states what rotation does not remediate, above the control', () => {
		const text = page();
		expect(prose(text), 'the caveat is named in full').toMatch(
			/This does not replace the master key\./
		);
		expect(
			text.indexOf('does not replace the master key'),
			'the caveat comes before the button'
		).toBeLessThan(text.indexOf('onclick={rotate}'));
	});

	it('is gated on a capability the server sends', () => {
		expect(page()).toContain('session.allowedGlobally().includes(actions.panelSettings)');
	});
});

// ADR-167 and 11 §9. A destination URL is a bearer credential the panel takes once and never
// shows again, and the address policy is the rule an operator will trip over — both have to be
// on the screen where the URL is typed, not discovered from a 422.
describe('the notifications screen', () => {
	const page = () =>
		readFileSync(join('src', 'routes', 'admin', 'webhooks', '+page.svelte'), 'utf8');

	it('says the URL is a credential the panel will not show again', () => {
		expect(prose(page())).toMatch(/stores it encrypted and never shows it again/);
	});

	it('states the address policy where the URL is entered', () => {
		const text = prose(page());
		expect(text, 'https only').toMatch(/Must be an <code>https:\/\/<\/code> address/);
		expect(text, 'private addresses and redirects').toMatch(
			/Addresses on this machine or its network are refused, and the panel does not follow redirects\./
		);
	});

	it('never renders a url field of its own', () => {
		expect(page(), 'a destination is named, never quoted back').not.toMatch(/w\.url/);
	});

	it('is gated on a capability the server sends', () => {
		expect(page()).toContain('session.allowedGlobally().includes(actions.panelSettings)');
	});

	it('says which events reach a destination, and which do not', () => {
		expect(prose(page())).toMatch(
			/stops on its own, a game update appears, or a backup fails\. A stop you asked for is not a notification\./
		);
	});
});

describe('the disk panel', () => {
	const detail = () =>
		readFileSync(join('src', 'routes', 'instances', '[id]', '+page.svelte'), 'utf8');

	// `03 §3.4`. Below its own floor the server runs normally and stops persisting the world:
	// no crash, no error, and the loss is found when someone reconnects. Footprint is the one
	// figure that does not move when that happens, so a panel showing only footprint shows
	// nothing about the failure it exists to prevent.
	it('03 §3.4 — remaining space is on screen, not only footprint', () => {
		const text = detail();
		expect(text, 'free space is rendered').toContain('disk.free_bytes');
		expect(text, 'and the alarm is the panel’s decision, not this component’s').toContain(
			'disk.low'
		);
		expect(text, 'the warning says what actually happens, not just that space is low').toMatch(
			/stops saving the world[\s\S]{0,80}silently/
		);
	});
});
