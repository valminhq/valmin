/**
 * The path a visitor was trying to reach before being sent to sign in, carried through as
 * `?next=`. Only a same-origin absolute path is honoured: anything else — an absolute URL, a
 * protocol-relative `//host`, a backslash the browser normalises to one — falls back to the
 * server list, so the parameter cannot be used to bounce someone off this origin.
 */
export function returnPath(url: URL): `/${string}` {
	const next = url.searchParams.get('next');
	if (!next || !next.startsWith('/')) return '/';
	if (next.startsWith('//') || next.startsWith('/\\')) return '/';
	return next as `/${string}`;
}

/** The sign-in URL, carrying the path being left so it can be returned to. */
export function loginWithReturn(login: `/${string}`, from: URL): `/${string}` {
	const next = from.pathname + from.search;
	if (next === '/') return login;
	return `${login}?next=${encodeURIComponent(next)}`;
}
