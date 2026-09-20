import { describe, expect, it } from 'vitest';
import { render } from 'svelte/server';
import FieldHarness from './field.test.svelte';

type Props = { id: string; label: string; hint?: string; error?: string; required?: boolean };
const body = (props: Props) => render(FieldHarness, { props }).body;

describe('field', () => {
	it('ties the label to the control', () => {
		const html = body({ id: 'server_name', label: 'Server name' });
		expect(html).toContain('for="server_name"');
		expect(html).toContain('id="server_name"');
	});

	it('describes the control by its hint alone when it is valid', () => {
		const html = body({ id: 'password', label: 'Password', hint: 'Players need this.' });
		expect(html).toContain('id="password-hint"');
		expect(html).toContain('aria-describedby="password-hint"');
		expect(html, 'a valid field is not invalid').not.toContain('aria-invalid');
	});

	it('marks the control invalid and describes it by hint and error together', () => {
		const html = body({
			id: 'password',
			label: 'Password',
			hint: 'Players need this.',
			error: 'At least 5 characters.'
		});
		expect(html).toContain('aria-invalid="true"');
		expect(html).toContain('aria-describedby="password-hint password-error"');
		expect(html).toContain('id="password-error"');
		expect(html, 'the correction is announced when it appears').toContain('role="alert"');
	});

	it('never names an element that is not rendered', () => {
		const onlyError = body({ id: 'name', label: 'Name', error: 'Required.' });
		expect(onlyError).toContain('aria-describedby="name-error"');
		expect(onlyError).not.toContain('name-hint');

		const neither = body({ id: 'name', label: 'Name' });
		expect(neither).not.toContain('aria-describedby');
	});

	it('marks a required field for both sighted and assistive readers', () => {
		const html = body({ id: 'name', label: 'Panel name', required: true });
		expect(html).toContain('(required)');
		expect(html).toContain('aria-hidden="true"');
	});
});
