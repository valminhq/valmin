/**
 * Orders overlapping reads of one view. Each call returns a check that stays true only until
 * the next call, so an older reply that lands late is dropped instead of overwriting a newer one.
 */
export function latestCall(): () => () => boolean {
	let count = 0;
	return () => {
		const mine = ++count;
		return () => mine === count;
	};
}
