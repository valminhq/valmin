import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api, csrfToken, request } from './client';
import { ApiError, NetworkError } from './errors';

function json(body: unknown, status = 200) {
	return new Response(JSON.stringify(body), {
		status,
		headers: { 'Content-Type': 'application/json', 'X-Request-Id': 'req-1' }
	});
}

beforeEach(() => {
	// The CSRF cookie is JS-readable by design (`11 §6.2`): the SPA has to echo it back in a
	// header, and a browser cannot read an HttpOnly one.
	vi.stubGlobal('document', { cookie: 'other=1; valmin_csrf=tok%2Fen; another=2' });
});

afterEach(() => {
	vi.unstubAllGlobals();
	vi.restoreAllMocks();
});

describe('csrf', () => {
	it('reads the cookie and decodes it', () => {
		expect(csrfToken()).toBe('tok/en');
	});

	it('sends the token on state-changing methods and not on reads', async () => {
		// A fresh Response per call: a Response body can only be read once.
		const fetchMock = vi.spyOn(globalThis, 'fetch').mockImplementation(async () => json({}));

		await api.get('/instances');
		await api.post('/instances/a/start');
		await api.patch('/instances/a', { name: 'x' });
		await api.del('/instances/a');

		const headers = fetchMock.mock.calls.map(
			(call) => (call[1]?.headers as Record<string, string>)['X-CSRF-Token']
		);
		expect(headers).toEqual([undefined, 'tok/en', 'tok/en', 'tok/en']);
	});
});

describe('the error envelope (`11 §2.1`)', () => {
	it('carries the code, the message and the request id', async () => {
		vi.spyOn(globalThis, 'fetch').mockResolvedValue(
			json(
				{
					error: {
						code: 'instance_must_be_stopped',
						message: 'The server must be stopped first.',
						details: { state: 'running' },
						request_id: 'req-9'
					}
				},
				409
			)
		);

		const failure = await api.post('/instances/a/mods').catch((e: unknown) => e);
		expect(failure).toBeInstanceOf(ApiError);
		const err = failure as ApiError;
		expect(err.code).toBe('instance_must_be_stopped');
		expect(err.status).toBe(409);
		// Surfaced in every error toast: the message is generic by design (D10) and the
		// chain that explains it is in the daemon's log under this id.
		expect(err.requestId).toBe('req-9');
		expect(err.details.state).toBe('running');
	});

	it('exposes per-field problems from a 422 (`11 §2.4`)', async () => {
		vi.spyOn(globalThis, 'fetch').mockResolvedValue(
			json(
				{
					error: {
						code: 'validation_failed',
						message: 'Some fields need fixing.',
						details: {
							fields: [{ field: 'password', code: 'too_short', message: 'At least 5 characters.' }]
						},
						request_id: 'req-2'
					}
				},
				422
			)
		);

		const err = (await api.post('/instances', {}).catch((e: unknown) => e)) as ApiError;
		expect(err.fields).toHaveLength(1);
		expect(err.field('password')).toBe('At least 5 characters.');
		expect(err.field('username')).toBeUndefined();
	});
});

// `11 §8.2`'s failure, from the client's side. The panel guarantees `/api` never falls
// back to index.html; if something in front of it answers with HTML anyway, saying so beats
// handing the body to a JSON parser and reporting a syntax error that names neither the URL
// nor the real problem.
it('refuses a response that is not an API response', async () => {
	vi.spyOn(globalThis, 'fetch').mockResolvedValue(
		new Response('<!doctype html><title>Valmin</title>', {
			status: 200,
			headers: { 'Content-Type': 'text/html' }
		})
	);

	const err = (await api.get('/instances').catch((e: unknown) => e)) as ApiError;
	expect(err).toBeInstanceOf(ApiError);
	expect(err.message).toContain('not an API response');
});

it('reports an unreachable panel as a network failure, not an API error', async () => {
	vi.spyOn(globalThis, 'fetch').mockRejectedValue(new TypeError('failed to fetch'));

	const err = await api.get('/instances').catch((e: unknown) => e);
	// A different type on purpose: there is no code and no request id to look up.
	expect(err).toBeInstanceOf(NetworkError);
	expect(err).not.toBeInstanceOf(ApiError);
});

it('returns nothing for a 204', async () => {
	vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response(null, { status: 204 }));
	await expect(request('/auth/logout', { method: 'POST' })).resolves.toBeUndefined();
});
