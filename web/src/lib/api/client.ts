import { ApiError, NetworkError, type ErrorEnvelope } from './errors';

/**
 * The panel's REST client. Hand-rolled, and that is the decision (`06 §4`, F6): live
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
	/** A body sent as `text/plain` instead of JSON. The raw `.cfg` routes carry the file's
	 * own bytes, which a JSON envelope would only put one escape layer away (`04 §3`). */
	text?: string;
	/** A multipart body. A world is hundreds of megabytes, so it is streamed as it is
	 * rather than encoded into a JSON string (`11 §8.3`). */
	form?: FormData;
	headers?: Record<string, string>;
	signal?: AbortSignal;
}

/** A text resource and the ETag that guards replacing it (`11 §1.1`). */
export interface TextResource {
	text: string;
	etag: string;
}

/** Sends one request. Decoding the answer is the caller's, because the panel serves two
 * kinds of body: JSON everywhere, and a config file's own text on the raw routes. */
async function send(path: string, options: RequestOptions): Promise<Response> {
	const method = options.method ?? 'GET';
	const headers: Record<string, string> = { ...options.headers };
	// A multipart body gets no Content-Type from here. The browser writes it, with the
	// boundary it generated; one set by hand names a boundary that is not in the body, and
	// the daemon's reader then finds no parts at all.
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

/** The failure a response describes, from the envelope it sent (`11 §2.1`) or a generic
 * one when whatever answered sent no envelope at all. */
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
 * Sends one request and returns the decoded body, or throws.
 *
 * A non-JSON body from `/api` is treated as a failure rather than parsed hopefully.
 * `11 §8.2` guarantees the SPA fallback never swallows an API path, so HTML arriving here
 * means something in front of the panel answered instead of the panel — and reporting it as
 * a JSON parse error names neither the URL nor the real problem.
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
	 * Replaces a text resource entirely.
	 *
	 * `etag` is a required argument and an empty one throws before the request is sent
	 * (`11 §1.1`, G1). A full replacement without it would discard whatever another writer
	 * saved in the meantime, and it would do so with a plausible cover story — the write
	 * succeeded, and nothing anywhere says what it took with it.
	 */
	putText: (path: string, text: string, etag: string) => {
		if (!etag) throw new Error('replacing a file needs the ETag from the read that loaded it');
		return textRequest(path, { method: 'PUT', text, headers: { 'If-Match': etag } });
	}
};
