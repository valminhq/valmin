// The error envelope of `11 §2.1`, as the SPA sees it. The codes are a closed registry
// (`11 §2.5`, ADR-034); the frontend never invents one and never parses `message` to decide
// what happened.

/** One entry of `details.fields` on a 422 (`11 §2.4`). */
export interface FieldError {
	field: string;
	code: string;
	message: string;
}

export interface ErrorEnvelope {
	error: {
		code: string;
		message: string;
		details?: Record<string, unknown>;
		request_id: string;
	};
}

/**
 * A failed request, carrying the envelope the panel sent.
 *
 * `↯` `requestId` is surfaced in every error toast on purpose. The generic message is all
 * the caller gets (D10); the `%w` chain that explains it is in the daemon's log under this
 * id, so an operator reporting "it said something went wrong" can be answered.
 */
export class ApiError extends Error {
	readonly status: number;
	readonly code: string;
	readonly details: Record<string, unknown>;
	readonly requestId: string;

	constructor(status: number, envelope: ErrorEnvelope['error']) {
		super(envelope.message);
		this.name = 'ApiError';
		this.status = status;
		this.code = envelope.code;
		this.details = envelope.details ?? {};
		this.requestId = envelope.request_id;
	}

	/** The per-field problems of a 422, or an empty array for any other failure. */
	get fields(): FieldError[] {
		const fields = this.details.fields;
		return Array.isArray(fields) ? (fields as FieldError[]) : [];
	}

	/** The message for one field, if this failure named it. */
	field(name: string): string | undefined {
		return this.fields.find((f) => f.field === name)?.message;
	}
}

/**
 * A request that never reached the panel — offline, DNS, a proxy that dropped it. It is
 * deliberately a different type from ApiError: there is no code, no request id, and
 * nothing to look up in a log.
 */
export class NetworkError extends Error {
	constructor(cause: unknown) {
		super('The panel could not be reached.');
		this.name = 'NetworkError';
		this.cause = cause;
	}
}
