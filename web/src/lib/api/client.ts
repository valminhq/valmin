import { ApiError, NetworkError, type ErrorEnvelope } from './errors';

/**
 * The panel's REST client, hand-rolled by decision (`06 §4`, F6). Live state arrives over the
 * WebSocket, so REST is initial loads and commands only.
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
	/** A body sent as `text/plain` instead of JSON: the raw `.cfg` routes carry the file's own
	 * bytes rather than a JSON envelope (`04 §3`). */
	text?: string;
	/** A multipart body, so a world of hundreds of megabytes streams as it is (`11 §8.3`). */
	form?: FormData;
	headers?: Record<string, string>;
	signal?: AbortSignal;
}

/** A text resource and the ETag that guards replacing it (`11 §1.1`). */
export interface TextResource {
	text: string;
	etag: string;
}

/** Sends one request. Decoding is the caller's: the panel serves JSON everywhere and a config
 * file's own text on the raw routes. */
async function send(path: string, options: RequestOptions): Promise<Response> {
	const method = options.method ?? 'GET';
	const headers: Record<string, string> = { ...options.headers };
	// A multipart body gets no Content-Type from here: only the browser knows the boundary it
	// generated.
	if (options.form !== undefined) delete headers['Content-Type'];
	else if (options.text !== undefined) headers['Content-Type'] = 'text/plain; charset=utf-8';
	else if (options.body !== undefined) headers['Content-Type'] = 'application/json';
	if (stateChanging(method)) headers[CSRF_HEADER] = csrfToken();

	try {
		return await fetch(BASE + path, {
			method,
			headers,
			credentials: 'same-origin',
			signal: options.signal,
			body:
				options.form ??
				options.text ??
				(options.body === undefined ? undefined : JSON.stringify(options.body))
		});
	} catch (cause) {
		throw new NetworkError(cause);
	}
}

/** The failure a response describes, from its envelope (`11 §2.1`) or a generic one when
 * whatever answered sent none. */
function failed(response: Response, payload?: unknown): ApiError {
	const envelope = (payload as Partial<ErrorEnvelope> | undefined)?.error;
	return new ApiError(
		response.status,
		envelope ?? {
			code: 'internal',
			message: 'Something went wrong.',
			request_id: response.headers.get('X-Request-Id') ?? ''
		}
	);
}

/**
 * Sends one request and returns the decoded body, or throws. A non-JSON body is a failure, not
 * something to parse hopefully: the SPA fallback never swallows an API path (`11 §8.2`), so it
 * means something in front of the panel answered instead.
 */
export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
	const response = await send(path, options);

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
	if (!response.ok) throw failed(response, payload);
	return payload as T;
}

/** Sends one request whose success is text and whose failure is still an envelope. */
async function textRequest(path: string, options: RequestOptions = {}): Promise<TextResource> {
	const response = await send(path, options);
	if (!response.ok) {
		const json = (response.headers.get('Content-Type') ?? '').includes('application/json');
		throw failed(response, json ? await response.json() : undefined);
	}
	return { text: await response.text(), etag: response.headers.get('ETag') ?? '' };
}

export const api = {
	get: <T>(path: string, signal?: AbortSignal) => request<T>(path, { signal }),
	post: <T>(path: string, body?: unknown) => request<T>(path, { method: 'POST', body }),
	upload: <T>(path: string, form: FormData) => request<T>(path, { method: 'POST', form }),
	patch: <T>(path: string, body?: unknown) => request<T>(path, { method: 'PATCH', body }),
	put: <T>(path: string, body?: unknown) => request<T>(path, { method: 'PUT', body }),
	del: <T>(path: string) => request<T>(path, { method: 'DELETE' }),

	getText: (path: string, signal?: AbortSignal) => textRequest(path, { signal }),
	/**
	 * Replaces a text resource entirely. `etag` is required and an empty one throws before the
	 * request is sent: a full replacement without it silently discards another writer's save
	 * (`11 §1.1`, G1).
	 */
	putText: (path: string, text: string, etag: string) => {
		if (!etag) throw new Error('replacing a file needs the ETag from the read that loaded it');
		return textRequest(path, { method: 'PUT', text, headers: { 'If-Match': etag } });
	}
};
