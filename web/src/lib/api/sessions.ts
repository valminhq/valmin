import { api } from './client';

/** One place the signed-in account is signed in, as `GET /me/sessions` reports it. */
export interface DeviceSession {
	id: string;
	created_at: string;
	last_seen_at: string;
	ip: string;
	user_agent: string;
	/** True for the session making the request. */
	current: boolean;
}

export const sessions = {
	list: () => api.get<{ items: DeviceSession[] }>('/me/sessions').then((page) => page.items),
	/** Signs one of the caller's own sessions out. */
	end: (id: string) => api.del<void>(`/me/sessions/${id}`)
};
