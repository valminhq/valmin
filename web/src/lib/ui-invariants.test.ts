import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs';
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
});

// Q25, closed: the code is blank in the registration line and carried by the session line the
// server logs once it is active. What replaced the ban on promising one is narrower — the
// panel shows a code the daemon read from a log, and shows nothing at all when it sent null.
// A placeholder here is worse than a blank, because it is a code someone will try to use.
describe('the crossplay join code', () => {
	it('holds no code of its own', () => {
		const text = readFileSync(join('src', 'lib', 'components', 'join-code.svelte'), 'utf8');
		expect(text).toMatch(/\{code\}/);
		expect(text, 'a sample code here is a code an operator will try').not.toMatch(/\d{4,}/);
	});
});

// The sightings are evidence about people, and the measured line that produces most of them
// is a connection attempt that may have been refused (docs/evidence/player-identity-2026-09-14.md).
// The screen must therefore not promise that a listed account played here, and it must show
// the id exactly as the daemon sent it: the three files accept the form the server printed,
// and rewriting one into the other silently strips an admin of admin (Q30).
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

// The server page — error-state recovery, the live join code, the clone link, free space, the
// player count and the console's command gating — is rendered and driven in
// routes/instances/[id]/page.svelte.test.ts, and the update notice beside its component.

// The dashboard, the clone screen and the adoption screen are rendered and driven in their own
// page.svelte.test.ts files.

// E7. The daemon sends null whenever it cannot tell — no reader, a stream that restarted, a
// peer that timed out without the server printing a new count. Rendering that as 0 would be a
// number an operator could act on, invented by the panel, so every reader of the field pairs
// it with a fallback that is not a number.
it('E7 — the player history never turns a gap into a count', () => {
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
	expect(view).toContain('First log entry');
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

// stripComments removes HTML comments until the text stops changing: a single pass leaves a
// `<!--` behind when one opener sits inside another.
const stripComments = (input: string): string => {
	let previous: string;
	do {
		previous = input;
		input = input.replace(/<!--[\s\S]*?-->/g, '');
	} while (input !== previous);
	return input;
};

// The mod screen. ADR-103 stands and is restated rather than quietly inherited: these read
// the source, not a browser. A button wired to nothing passes them.
// The mod screen's behaviour is rendered and driven in
// routes/instances/[id]/mods/page.svelte.test.ts. What stays here is about the SPA as a whole.
describe('the mod screen', () => {
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
			const body = stripComments(text)
				.replace(/\/\*[\s\S]*?\*\//g, '')
				.replace(/(^|[^:])\/\/.*$/gm, '$1');
			for (const pattern of forbidden) {
				if (pattern.test(body)) offenders.push(`${path} → ${pattern}`);
			}
		}
		expect(offenders, 'placement and loader vocabulary stays server-side (F2)').toEqual([]);
	});
});

// Q42. Mods are chosen in the create wizard because the wizard can start the server itself
// and the world is written on that first boot. The wizard's behaviour, its mods and its world step included, is rendered and driven in
// routes/instances/new/page.svelte.test.ts. What stays here is about the picker's source.
describe('the create wizard can install mods', () => {
	const picker = () => readFileSync(join('src', 'lib', 'components', 'mod-picker.svelte'), 'utf8');

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

// The config editor's behaviour is rendered and driven in routes/instances/[id]/configs, and
// the raw write's ETag rule in lib/api/client.test.ts. What stays here is about the source.
describe('the config editor', () => {
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

	it('the control never branches on the declared type', () => {
		expect(
			readFileSync(join('src', 'lib', 'components', 'config-setting.svelte'), 'utf8')
		).not.toMatch(/setting\.type ===/);
	});
});

// The settings screen's behaviour — what it sends, when it asks, what it gates — is rendered
// and driven in routes/instances/[id]/settings/page.svelte.test.ts. What stays here is the one
// claim about the source as a whole.
describe('the restart notice', () => {
	// B11 and ADR-118. The rebuild is stated conditionally, because a mod install sets the same
	// flag and triggers no rebuild (ADR-107). The claim lives in one component so a future
	// correction lands once; the mod screen keeps its own wording, which names mods.
	it('is the only place that claims a rebuild', () => {
		const claims = sources().filter(([, text]) => /rebuilds\s+the\s+container/.test(text));
		expect(
			claims.map(([path]) => path),
			'the rebuild is claimed in exactly one place'
		).toEqual([join('src', 'lib', 'components', 'restart-notice.svelte')]);
	});
});

// The player lists, the world import panel and the schedules editor are rendered and driven in
// their own *.svelte.test.ts files beside them.

// A copy action that fails silently is a value the operator believes they have. The clipboard
// API is unavailable over plain HTTP, which is how a panel on a LAN address is reached, so the
// refusal is a state the panel renders rather than an exception nobody catches.
describe('copying a value', () => {
	it('is one component, and nothing reaches the clipboard around it', () => {
		const offenders: string[] = [];
		for (const [path, text] of sources()) {
			if (path.endsWith(join('components', 'copy-button.svelte'))) continue;
			if (text.includes('navigator.clipboard')) offenders.push(path);
		}
		expect(offenders, 'use <CopyButton value={…} /> instead').toEqual([]);
	});
});

// The backups panel's behaviour is rendered and driven in
// lib/components/backups-panel.svelte.test.ts. What stays here is about the source around it.
describe('the backups panel', () => {
	// The catalogue had rows and no way to see them. A panel nothing links to is the same
	// failure with more code in it.
	it('is reachable from the server it belongs to', () => {
		expect(
			readFileSync(join('src', 'routes', 'instances', '[id]', 'backups', '+page.svelte'), 'utf8')
		).toContain('<BackupsPanel');
		expect(
			readFileSync(join('src', 'lib', 'components', 'server-nav.svelte'), 'utf8'),
			'server navigation links to it'
		).toContain('/instances/[id]/backups');
	});

	// F9's rule: a fact that exists only in a tooltip is one a keyboard and a touch screen do
	// not have, and whether an archive can be trusted is not a fact to hide there.
	it('leaves nothing to a tooltip', () => {
		expect(
			readFileSync(join('src', 'lib', 'components', 'backups-panel.svelte'), 'utf8')
		).not.toMatch(/<Badge[^>]*title=/);
	});
});

describe('the schedules editor', () => {
	// The expression is the daemon's to validate: it answers an invalid one with the field and
	// its own help. A second parser here would drift from the one that actually runs.
	it('the cron expression is not parsed in the SPA', () => {
		expect(
			readFileSync(join('src', 'lib', 'components', 'schedules-editor.svelte'), 'utf8'),
			'nothing splits or interprets the expression'
		).not.toMatch(/cron\.(split|match|test)|parseCron|cronParse/);
	});
});

describe('WP-M6-03 asynchronous component boundaries', () => {
	it('constructs the console virtualizer without tracking row count', () => {
		const text = readFileSync(join('src', 'lib', 'components', 'console-view.svelte'), 'utf8');
		expect(text).toContain("import { untrack } from 'svelte'");
		expect(text).toContain('untrack(() => buffer.rows.length)');
	});

	it('tracks both config route parameters before loading', () => {
		const text = readFileSync(
			join('src', 'routes', 'instances', '[id]', 'configs', '[file]', '+page.svelte'),
			'utf8'
		);
		expect(text).toMatch(/void load\(id, file\)/);
		expect(text).toMatch(/async function load\(targetID: string, targetFile: string\)/);
	});

	// The mods page's guard is driven in its page test; the wizard's picker shares the shape.
	it('rejects stale results from the wizard’s catalogue search', () => {
		const text = readFileSync(join('src', 'lib', 'components', 'mod-picker.svelte'), 'utf8');
		expect(text).toMatch(/const request = \+\+searchRequest/);
		expect(text).toMatch(/if \(request !== searchRequest\) return/);
	});
});

// ADR-195. One condition, one place on the screen. A server's own conditions are chips on
// its card; what has no card sits in one host band; and nothing restates a badge the card
// already carries.
describe('the server list conditions', () => {
	// Which conditions reach a card, their order and the host band are unit-tested in
	// conditions.test.ts, and their placement on the dashboard in routes/page.svelte.test.ts.
	it('has no second aggregated list restating what the cards show', () => {
		expect(
			readFileSync(join('src', 'routes', '+page.svelte'), 'utf8'),
			'the separate inbox card is gone'
		).not.toMatch(/OperationsInbox/);
		expect(
			existsSync(join('src', 'lib', 'components', 'operations-inbox.svelte')),
			'the component it rendered is gone too'
		).toBe(false);
	});
});

// A read that failed and a read that returned nothing are different facts. The screens that
// confuse them go on to make claims about disk and health that a transport error does not
// license.
describe('a failed read is never rendered as an empty result', () => {
	it('a catch that empties a collection also records the failure', () => {
		const emptied = /catch\s*(?:\([^)]*\))?\s*\{([^}]*=\s*\[\][^}]*)\}/g;
		const offenders: string[] = [];
		for (const [path, text] of sources()) {
			for (const [, body] of text.matchAll(emptied)) {
				if (!/ailure|error/i.test(body)) offenders.push(path);
			}
		}
		expect(offenders, 'emptying a collection on a rejection discards the failure').toEqual([]);
	});

	it('a rejection is never discarded by a bare arrow that assigns an empty array', () => {
		const discarded = /\.catch\(\s*\(\s*\)\s*=>\s*\(?[\w.]+\s*=\s*\[\]/;
		const offenders = sources()
			.filter(([, text]) => discarded.test(text))
			.map(([path]) => path);
		expect(offenders).toEqual([]);
	});
});

// A page's identity and its landmarks are what a screen reader, a browser tab and a keyboard
// navigate by, and all three were the same on every authenticated screen.
describe('every page says which page it is', () => {
	const layout = () => readFileSync(join('src', 'routes', '+layout.svelte'), 'utf8');
	const pages = () =>
		sources().filter(([path]) => path.endsWith(join('+page.svelte')) && path.includes('routes'));

	it('renders exactly one main landmark, counting the one the server layout supplies', () => {
		const serverLayout = readFileSync(
			join('src', 'routes', 'instances', '[id]', '+layout.svelte'),
			'utf8'
		);
		expect(serverLayout, 'the section pages inherit it').toContain('<main>');

		const wrong = pages().filter(([path, text]) => {
			const own = (text.match(/<main[\s>]/g) ?? []).length;
			const inherited = path.includes(join('instances', '[id]')) ? 1 : 0;
			return own + inherited !== 1;
		});
		expect(wrong.map(([path]) => path)).toEqual([]);
	});

	it('titles pages from one table, so two servers on a section are distinguishable', () => {
		expect(layout()).toMatch(/const SECTION: Record<string, string>/);
		expect(layout()).toMatch(/\[serverName, section, 'Valmin'\]\.filter\(Boolean\)\.join\(' · '\)/);
	});

	it('leaves a route its own title only when the table does not name it', () => {
		const owned = pages().filter(([, text]) => /<svelte:head>[\s\S]{0,200}<title>/.test(text));
		expect(owned.map(([path]) => path).sort()).toEqual(
			[
				join('src', 'routes', 'redeem', '[token]', '+page.svelte'),
				join('src', 'routes', 'status', '[id]', '+page.svelte')
			].sort()
		);
		// Two titles would render, and the shell's would not be the one in the tab.
		expect(layout()).not.toMatch(/'\/status\/\[id\]':/);
		expect(layout()).not.toMatch(/'\/redeem\/\[token\]':/);
	});

	it('offers a skip link into a target that can take focus', () => {
		expect(layout()).toContain('href="#main"');
		expect(layout()).toMatch(/<div id="main" tabindex="-1">/);
	});

	it('names the server once, above its sections, rather than once per section', () => {
		const serverLayout = readFileSync(
			join('src', 'routes', 'instances', '[id]', '+layout.svelte'),
			'utf8'
		);
		expect(serverLayout).toMatch(/<h1 class="[^"]*">\{instance\?\.name \?\? 'Server'\}<\/h1>/);
		expect(serverLayout, 'a deep link never ran the dashboard load').toContain(
			'instanceList.ensure()'
		);

		const sections = pages().filter(([path]) => path.includes(join('instances', '[id]')));
		const shouting = sections.filter(([, text]) => text.includes('<h1'));
		expect(
			shouting.map(([path]) => path),
			'a section is an h2 under the server'
		).toEqual([]);
	});
});

// The key rotation, notifications and diagnostics screens are rendered and driven in their
// page.svelte.test.ts files under routes/admin.

// Two registries feed one catalogue (`03 §6.1`, ADR-210), and they can serve different bytes
// under one ident (B14). Every assertion here guards a failure that renders perfectly: a
// switch that never refetches, two rows Svelte refuses to key apart, or a colour spelled out
// in one screen and not the other.
describe('the mod registry switch', () => {
	const modsPage = () =>
		readFileSync(join('src', 'routes', 'instances', '[id]', 'mods', '+page.svelte'), 'utf8');
	const picker = () => readFileSync(join('src', 'lib', 'components', 'mod-picker.svelte'), 'utf8');

	// One package on both registries is two rows with one `full_name`. Keyed on the name
	// alone, Svelte throws on a duplicate key the first time that happens — a runtime crash on
	// a real catalogue, invisible to any fixture carrying one registry.
	//
	// The installed list is deliberately not covered: an instance holds a package once, from
	// one registry (B14), so `full_name` is unique there and is the right key.
	it('catalogue lists key on the registry as well as the package', () => {
		const lists: Array<[where: string, list: string, text: string]> = [
			['the mods page', 'results', modsPage()],
			['the mods page', 'pending.nodes', modsPage()],
			['the picker', 'results', picker()],
			['the picker', 'chosen', picker()]
		];
		for (const [where, list, text] of lists) {
			const each = text.match(
				new RegExp(`\\{#each ${list.replace('.', '\\.')} as \\w+ \\(([^)]*)\\)`)
			);
			expect(each, `${where}: the ${list} list is still there`).not.toBeNull();
			expect(each?.[1], `${where}: ${list} keys on the registry too`).toContain('source');
		}
	});

	// The colours live in one module so the two mod screens cannot drift apart, and `app.css`
	// is vendored shadcn output that gains no tokens of its own (ADR-002). Scoped to the mod
	// screens: emerald and amber are already the house palette for run state and transient
	// conditions elsewhere, and those uses predate any registry.
	it('registry colours come from the shared map, never spelled out per screen', () => {
		for (const [where, text] of [
			['the mods page', modsPage()],
			['the picker', picker()]
		] as const) {
			expect(text, `${where} does not spell out a registry colour`).not.toMatch(
				/\b(?:bg|text|border)-(?:sky|amber)-\d{2,3}\b/
			);
		}
		expect(
			readFileSync(join('src', 'app.css'), 'utf8'),
			'and app.css gains no registry tokens'
		).not.toContain('--registry');
	});

	// Colour alone excludes a colour-blind operator and dies in a greyscale screenshot, so
	// every screen that colours a version also names the registry.
	it('every screen that colours a version also names the registry', () => {
		for (const [name, text] of [
			['the mods page', modsPage()],
			['the picker', picker()]
		] as const) {
			expect(text, `${name} colours the version`).toContain('sourceText[');
			expect(text, `${name} also names the registry`).toContain('sourceLabel[');
		}
	});
});
