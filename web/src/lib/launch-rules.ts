// Browser copies of the daemon's launch and join-address rules, so a form can say what is wrong
// before it is sent. The daemon checks again; these only spare a round trip.

/** Whether password appears inside the server or world name, as the game refuses it. */
export function passwordInName(password: string, serverName: string, worldName: string): boolean {
	return password !== '' && (serverName.includes(password) || worldName.includes(password));
}

/** One DNS label: 1 to 63 letters, digits and hyphens, not starting or ending with a hyphen. */
const LABEL = /^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$/;

/** Whether host is an IPv4 address, an IPv6 address or a DNS name, with no port or scheme. */
export function validJoinHost(host: string): boolean {
	if (host.includes(':')) return /^[0-9A-Fa-f:.]+$/.test(host) && host.split(':').length > 2;
	return host.length <= 253 && host.split('.').every((label) => LABEL.test(label));
}
