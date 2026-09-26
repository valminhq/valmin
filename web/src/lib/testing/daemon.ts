import { vi } from 'vitest';
import type { GameOptions, Instance } from '$lib/api/instances';
import type { Backup } from '$lib/api/backups';
import type { Job, MyPermissions } from '$lib/api/types';

/** One request the screen under test sent, as the daemon received it. */
export interface Sent {
	method: string;
	/** Below `/api/v1`, without the query string: routes match on this. */
	path: string;
	query: URLSearchParams;
	headers: Record<string, string>;
	/** Parsed when the page sent JSON; the text itself for a raw `.cfg` write. */
	body: unknown;
}

type Handler = (request: Sent) => Response | Promise<Response>;

/**
 * A stand-in for the daemon behind `fetch`, so a screen test drives the real API client and
 * asserts on the requests that left the page — what the daemon would have acted on — rather
 * than on the code meant to build them. An unrouted request answers 404 with the panel's
 * envelope, the way the daemon answers a path it does not serve.
 */
export class FakeDaemon {
	readonly sent: Sent[] = [];
	private readonly routes = new Map<string, Handler>();

	/** Answers `method path` (the path below `/api/v1`) with handler, replacing any earlier one. */
	on(method: string, path: string, handler: Handler): this {
		this.routes.set(`${method} ${path}`, handler);
		return this;
	}

	/** Every request of method to path, in the order the screen sent them. */
	requests(method: string, path: string): Sent[] {
		return this.sent.filter((r) => r.method === method && r.path === path);
	}

	install(): void {
		vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
			const url = new URL(String(input), 'http://localhost');
			const headers = { ...(init?.headers as Record<string, string> | undefined) };
			const json = (headers['Content-Type'] ?? '').includes('application/json');
			const request: Sent = {
				method: init?.method ?? 'GET',
				path: url.pathname.replace(/^\/api\/v1/, ''),
				query: url.searchParams,
				headers,
				body: json && typeof init?.body === 'string' ? JSON.parse(init.body) : init?.body
			};
			this.sent.push(request);
			const handler = this.routes.get(`${request.method} ${request.path}`);
			return handler ? handler(request) : envelope(404, 'not_found', 'Not found.');
		});
	}
}

/** The panel's error envelope (`11 §2.1`), with per-field problems on a 422 (`11 §2.4`). */
export function envelope(
	status: number,
	code: string,
	message: string,
	fields?: Array<{ field: string; code: string; message: string }>
): Response {
	return Response.json(
		{ error: { code, message, request_id: 'req-test', details: fields ? { fields } : undefined } },
		{ status }
	);
}

export function instance(overrides: Partial<Instance> = {}): Instance {
	return {
		id: 'inst-a',
		name: 'inst-a',
		state: 'stopped',
		base_port: 2456,
		server_name: 'My Server',
		world_name: 'MyWorld',
		public: false,
		crossplay: false,
		crossplay_instance_id: '',
		crossplay_join_code: null,
		modded: false,
		restart_required: false,
		mem_limit_mb: 4096,
		cpu_limit: null,
		backup_keep_cold: 7,
		backup_keep_hot: 3,
		backup_on_restart: false,
		status_published: false,
		created_at: '2026-09-01T00:00:00Z',
		updated_at: '2026-09-01T00:00:00Z',
		...overrides
	};
}

export function gameOptions(overrides: Partial<GameOptions> = {}): GameOptions {
	return {
		build: '21981590',
		presets: ['casual', 'hard'],
		presets_complete: false,
		modifier_keys: ['combat'],
		modifier_values_measured: false,
		save_defaults: {
			save_interval_seconds: 1800,
			backups: 4,
			backup_short_seconds: 7200,
			backup_long_seconds: 43200
		},
		crossplay_untested: [],
		min_password_length: 5,
		min_memory_limit_mb: 2048,
		...overrides
	};
}

/** The permission set a signed-in member holding exactly `actions` on one instance sees, and
 * `global` for what belongs to no instance — creating a server, for one. */
export function permissions(
	instanceId: string,
	actions: string[],
	global: string[] = []
): MyPermissions {
	return {
		user_id: 'u-test',
		role: 'member',
		allowed_actions: global,
		instances: [{ instance_id: instanceId, allowed_actions: actions }]
	};
}

export function backup(overrides: Partial<Backup> = {}): Backup {
	return {
		id: 'bk-1',
		instance_id: 'inst-a',
		size_bytes: 3 * 1024 * 1024,
		sha256: 'ab',
		world_name: 'MyWorld',
		trigger: 'manual',
		consistent: true,
		created_at: '2026-09-20T12:00:00Z',
		filename: 'inst-a-MyWorld-20260920T120000Z-bk-1.tar.gz',
		prunes_next: false,
		...overrides
	};
}

export function job(overrides: Partial<Job> = {}): Job {
	return {
		job_id: 'job-1',
		kind: 'backup',
		status: 'running',
		progress: 0,
		created_at: '2026-09-20T12:00:00Z',
		...overrides
	};
}

/** A text resource as the daemon serves a raw `.cfg`: the bytes and the ETag guarding them. */
export function textResource(text: string, etag: string): Response {
	return new Response(text, {
		headers: { 'Content-Type': 'text/plain; charset=utf-8', ETag: etag }
	});
}
