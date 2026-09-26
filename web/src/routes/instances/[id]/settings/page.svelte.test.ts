import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { actions } from '$lib/api/instances';
import { session } from '$lib/state/session.svelte';
import { click } from '$lib/testing/interact';
import { FakeDaemon, envelope, gameOptions, instance, permissions } from '$lib/testing/daemon';
import Page from './+page.svelte';

vi.mock('$app/state', () => ({
	page: { params: { id: 'inst-a' }, url: new URL('http://localhost/instances/inst-a/settings') }
}));

let daemon: FakeDaemon;

/** Renders the screen for a member holding `held` on inst-a, once its first load has settled. */
async function open(held: string[], row = instance()) {
	daemon.on('GET', '/instances/inst-a', () => Response.json(row));
	daemon.on('GET', '/game/options', () => Response.json(gameOptions()));
	session.permissions = permissions('inst-a', held);
	render(Page);
	await screen.findByLabelText('Server name');
}

async function type(label: string, value: string) {
	await fireEvent.input(screen.getByLabelText(label), { target: { value } });
}

const saveButton = () => screen.getByRole('button', { name: 'Save changes' });
const patches = () => daemon.requests('PATCH', '/instances/inst-a');

beforeEach(() => {
	daemon = new FakeDaemon();
	daemon.install();
});

afterEach(() => {
	session.permissions = null;
	vi.unstubAllGlobals();
});

describe('the server settings screen', () => {
	// PATCH semantics (`11 §1.1`): absent means unchanged. A form that sent every field would
	// re-encrypt an untouched password on every save and rewrite settings nobody edited.
	it('sends only the fields the operator changed', async () => {
		await open([actions.settings]);
		daemon.on('PATCH', '/instances/inst-a', () =>
			Response.json(instance({ server_name: 'Renamed' }))
		);

		expect((saveButton() as HTMLButtonElement).disabled, 'nothing changed yet').toBe(true);
		await type('Server name', '  Renamed  ');
		await click(saveButton());

		await vi.waitFor(() => expect(patches()).toHaveLength(1));
		expect(patches()[0].body, 'one field, trimmed, and nothing else').toEqual({
			server_name: 'Renamed'
		});
	});

	// F4. The row on screen after a save is the one the daemon returned, never the one the
	// form sent: a normalised or rejected value would otherwise be shown as saved.
	it('shows the row the daemon returned after a save', async () => {
		await open([actions.settings]);
		daemon.on('PATCH', '/instances/inst-a', () =>
			Response.json(instance({ server_name: 'Normalised By Daemon' }))
		);

		await type('Server name', 'typed by operator');
		await click(saveButton());

		await vi.waitFor(() =>
			expect((screen.getByLabelText('Server name') as HTMLInputElement).value).toBe(
				'Normalised By Daemon'
			)
		);
		expect(screen.getByText('Nothing to save.')).toBeTruthy();
	});

	// F5. A new password locks every player out until someone tells them, and the panel
	// cannot. It is the one field here that asks first.
	it('asks before changing the password, and sends nothing until confirmed', async () => {
		await open([actions.settings]);
		daemon.on('PATCH', '/instances/inst-a', () => Response.json(instance()));

		await type('Server password', 'correct-horse');
		await click(saveButton());

		const dialog = await screen.findByRole('dialog');
		expect(dialog.textContent, 'it names the consequence').toMatch(/needs the new password/);
		expect(patches(), 'nothing is sent while the question is open').toHaveLength(0);

		await click(within(dialog).getByRole('button', { name: 'Cancel' }));
		expect(patches(), 'cancelling sends nothing').toHaveLength(0);

		await click(saveButton());
		await click(
			within(await screen.findByRole('dialog')).getByRole('button', {
				name: 'Save changes and password'
			})
		);
		await vi.waitFor(() => expect(patches()).toHaveLength(1));
		expect(patches()[0].body).toEqual({ password: 'correct-horse' });
	});

	it('saves other fields without asking', async () => {
		await open([actions.settings]);
		daemon.on('PATCH', '/instances/inst-a', () => Response.json(instance({ public: true })));

		await click(screen.getByRole('switch', { name: 'List publicly' }));
		await click(saveButton());

		await vi.waitFor(() => expect(patches()).toHaveLength(1));
		expect(screen.queryByRole('dialog')).toBeNull();
		expect(patches()[0].body).toEqual({ public: true });
	});

	// F3. Ordinary settings and resource limits are gated separately (ADR-121), each on the
	// action the daemon sends and never on a role.
	it('gates settings and limits on their own actions', async () => {
		await open([actions.settings]);

		expect((screen.getByLabelText('Server name') as HTMLInputElement).disabled).toBe(false);
		expect((screen.getByLabelText('Memory limit (MB)') as HTMLInputElement).disabled).toBe(true);
		expect((screen.getByLabelText('CPU limit (cores)') as HTMLInputElement).disabled).toBe(true);
		expect(screen.getByText(/require the separate limit-management capability/)).toBeTruthy();
	});

	it('lets a holder of limits alone change limits and nothing else', async () => {
		await open([actions.limits]);
		daemon.on('PATCH', '/instances/inst-a', () => Response.json(instance({ mem_limit_mb: 8192 })));

		expect((screen.getByLabelText('Server name') as HTMLInputElement).disabled).toBe(true);
		await type('Memory limit (MB)', '8192');
		await click(saveButton());

		await vi.waitFor(() => expect(patches()).toHaveLength(1));
		expect(patches()[0].body).toEqual({ mem_limit_mb: 8192 });
	});

	it('offers no save to someone who can only look', async () => {
		await open([actions.view]);

		expect(screen.getByText('You can see these settings but not change them.')).toBeTruthy();
		expect(screen.queryByRole('button', { name: 'Save changes' })).toBeNull();
		for (const label of ['Server name', 'Server password', 'Memory limit (MB)']) {
			expect((screen.getByLabelText(label) as HTMLInputElement).disabled, label).toBe(true);
		}
	});

	// Q41: the import endpoint shipped with nothing calling it. It is reached from here.
	it('carries the world import panel for a holder of world.import', async () => {
		await open([actions.settings, actions.worldImport]);
		expect(screen.getByText('Import a world')).toBeTruthy();
		expect(screen.getByLabelText('World folder')).toBeTruthy();
	});

	// Q48. `-world` names the save file, so renaming it moves files rather than writing a
	// column. An operator who cannot find the setting concludes the panel is broken; one who
	// is told why concludes it is honest.
	it('shows the world name locked, with the reason', async () => {
		await open([actions.settings]);

		const world = screen.getByLabelText('World name') as HTMLInputElement;
		expect(world.value).toBe('MyWorld');
		expect(world.readOnly && world.disabled).toBe(true);
		expect(screen.getByText(/name of the save file on disk/)).toBeTruthy();
	});

	// Q49 (E8). What a changed preset does to an existing world is unmeasured, so the screen
	// claims neither safety nor harm.
	it('says the effect of presets and modifiers on a world is unverified', async () => {
		await open([actions.settings]);
		expect(screen.getByText(/effects on existing worlds have not been verified/)).toBeTruthy();
	});

	// B11. A saved launch edit applies on the next start, and the screen says so whenever the
	// daemon reports the running server is behind.
	it('shows the restart notice only when the daemon says a restart is needed', async () => {
		await open([actions.settings], instance({ restart_required: true }));
		expect(screen.getByText('Restart required')).toBeTruthy();
	});

	it('shows no restart notice for a server that is up to date', async () => {
		await open([actions.settings]);
		expect(screen.queryByText('Restart required')).toBeNull();
	});

	// 11 §2.4. A rejected field is rendered beside the input it belongs to, tied to it for
	// assistive technology, rather than as one message at the top.
	it('renders a rejected limit beside its input', async () => {
		await open([actions.settings, actions.limits]);
		daemon.on('PATCH', '/instances/inst-a', () =>
			envelope(422, 'validation_failed', 'Some settings are invalid.', [
				{ field: 'mem_limit_mb', code: 'too_small', message: 'This host cannot give that much.' }
			])
		);

		await type('Memory limit (MB)', '65536');
		await click(saveButton());

		const input = screen.getByLabelText('Memory limit (MB)');
		await vi.waitFor(() => expect(input.getAttribute('aria-invalid')).toBe('true'));
		const describedBy = (input.getAttribute('aria-describedby') ?? '').split(' ');
		const messages = describedBy.map((id) => document.getElementById(id)?.textContent ?? '');
		expect(messages.join(' ')).toContain('This host cannot give that much.');
		await vi.waitFor(() => expect(document.activeElement, 'focus moves to it').toBe(input));
	});

	it('refuses a password the daemon would reject before sending it', async () => {
		await open([actions.settings]);

		await type('Server password', 'abc');
		expect((saveButton() as HTMLButtonElement).disabled).toBe(true);
		expect(screen.getByText('At least 5 characters.')).toBeTruthy();
		expect(patches()).toHaveLength(0);
	});
});
