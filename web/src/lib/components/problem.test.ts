import { describe, expect, it } from 'vitest';
import { render } from 'svelte/server';
import { ApiError, NetworkError } from '$lib/api/errors';
import Problem from './problem.svelte';

describe('visible errors', () => {
	it('renders validation details and a support reference', () => {
		const error = new ApiError(422, {
			code: 'validation_failed',
			message: 'Some values are invalid. Check the validation details.',
			details: {
				fields: [
					{ field: 'server_name', code: 'required', message: 'Enter a server name.' },
					{ field: 'password', code: 'too_short', message: 'Use at least 5 characters.' }
				]
			},
			request_id: 'req-validation'
		});
		const { body } = render(Problem, { props: { error } });
		expect(body).toContain('server name:');
		expect(body).toContain('Enter a server name.');
		expect(body).toContain('Use at least 5 characters.');
		expect(body).toContain('Request ID:');
		expect(body).toContain('req-validation');
	});

	it('explains connection failures without exposing their cause', () => {
		const error = new NetworkError(new Error('private proxy details'));
		const { body } = render(Problem, { props: { error } });
		expect(body).toContain('Cannot connect to Valmin.');
		expect(body).not.toContain('private proxy details');
		expect(body).not.toContain('Request ID:');
	});

	it('keeps unexpected errors generic', () => {
		const { body } = render(Problem, { props: { error: new Error('/private/path') } });
		expect(body).toContain('Valmin could not complete the action.');
		expect(body).not.toContain('/private/path');
	});

	it('renders no alert without an error', () => {
		const { body } = render(Problem, { props: { error: null } });
		expect(body).not.toContain('role="alert"');
		expect(body).not.toContain('Request ID:');
	});
});
