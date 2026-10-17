import type { AlertRule, RuleKind } from '$lib/api/admin';

/** One event a rule can send, as the notification pages show it. The headline, colour and default
 * message mirror what the daemon sends (notify.headline, notify.accent, delivery.defaultTemplates). */
export interface NotifyEvent<K extends RuleKind = RuleKind, G extends string = string> {
	kind: K;
	group: G;
	label: string;
	/** When it is sent, in a reader's words. */
	when: string;
	headline: string;
	/** The Discord card's colour. */
	accent: string;
	/** The message a rule without a template sends. */
	template: string;
	/** The placeholders its template can name. */
	placeholders: string[];
	/** Placeholder values for the preview. */
	sample: Record<string, string>;
}

/** The longest template the daemon stores, in characters. */
export const MAX_TEMPLATE = 1000;

/**
 * Replaces each {name} with its value. A name without one becomes empty, and a line whose every
 * placeholder came out empty is left out, as the daemon does.
 */
export function expand(template: string, vars: Record<string, string>): string {
	const kept: string[] = [];
	for (const line of template.split('\n')) {
		let named = false;
		let filled = false;
		const out = line.replace(/\{([a-z_]+)\}/g, (_, name: string) => {
			const value = vars[name] ?? '';
			named = true;
			filled ||= value !== '';
			return value;
		});
		if (!named || filled) kept.push(out);
	}
	return kept.join('\n');
}

/** How a Discord card is laid out: '' titles it with the server and the event on one line. */
export type Layout = '' | 'message' | 'plain';

export interface MessageStyle {
	layout: Layout;
	hideLink: boolean;
	hideTime: boolean;
	/** #RRGGBB, or '' for the event's own colour. */
	color: string;
}

export const DEFAULT_STYLE: MessageStyle = {
	layout: '',
	hideLink: false,
	hideTime: false,
	color: ''
};

export const LAYOUTS: { value: Layout; label: string }[] = [
	{ value: '', label: 'One line' },
	{ value: 'message', label: 'Message only' },
	{ value: 'plain', label: 'Plain text' }
];

/** A rule's stored style. */
export function styleOf(params: AlertRule['params']): MessageStyle {
	return {
		layout: params.layout ?? '',
		hideLink: params.hide_link ?? false,
		hideTime: params.hide_time ?? false,
		color: params.color ?? ''
	};
}

/** A style as rule params; a default is left out, as the daemon reads it. */
export function styleParams(style: MessageStyle): AlertRule['params'] {
	const p: AlertRule['params'] = {};
	if (style.layout) p.layout = style.layout;
	if (style.hideLink) p.hide_link = true;
	if (style.hideTime) p.hide_time = true;
	if (style.color) p.color = style.color;
	return p;
}

export const sameStyle = (a: MessageStyle, b: MessageStyle) =>
	JSON.stringify(styleParams(a)) === JSON.stringify(styleParams(b));

/** The most frequent of keys, the earliest on a tie, or undefined for none. */
export function mostCommon(keys: string[]): string | undefined {
	const counts = new Map<string, number>();
	for (const key of keys) counts.set(key, (counts.get(key) ?? 0) + 1);
	let best: string | undefined;
	for (const [key, n] of counts) if (best === undefined || n > counts.get(best)!) best = key;
	return best;
}
