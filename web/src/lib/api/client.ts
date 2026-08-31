import { ApiError, NetworkError, type ErrorEnvelope } from './errors';

/**
 * The panel's REST client. `↯` Hand-rolled, and that is the decision (`06 §4`, F6): live
 * state arrives over the WebSocket, so REST is initial loads and commands — there is no
 * cache to invalidate, no query key to get wrong, and nothing a data-fetching library would
 * be doing for us.
 */
const BASE = '/api/v1';

/** Where the double-submit CSRF cookie lives (`11 §6.2`). It is readable by JS by design. */
const CSRF_COOKIE = 'valmin_csrf';
const CSRF_HEADER = 'X-CSRF-Token';

export function csrfToken(): string {
	for (const part of document.cookie.split(';')) {
		const [name, ...rest] = part.trim().split('=');
		if (name === CSRF_COOKIE) return decodeURIComponent(rest.join('='));
	}
	return '';
}

function stateChanging(method: string): boolean {
	return method === 'POST' || method === 'PUT' || method === 'PATCH' || method === 'DELETE';
}

interface RequestOptions {
	method?: string;
	body?: unknown;
	signal?: AbortSignal;
}

/**
 * Sends one request and returns the decoded body, or throws.
 *
 * `↯` A non-JSON body from `/api` is treated as a failure rather than parsed hopefully.
 * `11 §8.2` guarantees the SPA fallback never swallows an API path, so HTML arriving here
 * means something in front of the panel answered instead of the panel — and reporting it as
 * a JSON parse error names neither the URL nor the real problem.
 */
export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
	const method = options.method ?? 'GET';
	const headers: Record<string, string> = {};
	if (options.body !== undefined) headers['Content-Type'] = 'application/json';
	if (stateChanging(method)) headers[CSRF_HEADER] = csrfToken();

	let response: Response;
	try {
		response = await fetch(BASE + path, {
			method,
			headers,
			credentials: 'same-origin',
			signal: options.signal,
			body: options.body === undefined ? undefined : JSON.stringify(options.body)
		});
	} catch (cause) {
		throw new NetworkError(cause);
	}

	if (response.status === 204) return undefined as T;

	const contentType = response.headers.get('Content-Type') ?? '';
	if (!contentType.includes('application/json')) {
		throw new ApiError(response.status, {
			code: 'internal',
			message: 'The panel returned something that is not an API response.',
			request_id: response.headers.get('X-Request-Id') ?? ''
		});
	}

	const payload = (await response.json()) as unknown;
	if (!response.ok) {
		const envelope = payload as Partial<ErrorEnvelope>;
		throw new ApiError(
			response.status,
			envelope.error ?? {
				code: 'internal',
				message: 'Something went wrong.',
				request_id: response.headers.get('X-Request-Id') ?? ''
			}
		);
	}
	return payload as T;
}

export const api = {
	get: <T>(path: string, signal?: AbortSignal) => request<T>(path, { signal }),
	post: <T>(path: string, body?: unknown) => request<T>(path, { method: 'POST', body }),
	patch: <T>(path: string, body?: unknown) => request<T>(path, { method: 'PATCH', body }),
	put: <T>(path: string, body?: unknown) => request<T>(path, { method: 'PUT', body }),
	del: <T>(path: string) => request<T>(path, { method: 'DELETE' })
};
