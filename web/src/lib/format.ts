/** A byte count in binary units, one decimal above bytes: "512 B", "3.4 MiB". */
export function formatBytes(n: number): string {
	const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
	let v = n;
	let u = 0;
	while (v >= 1024 && u < units.length - 1) {
		v /= 1024;
		u += 1;
	}
	return `${v.toFixed(u === 0 ? 0 : 1)} ${units[u]}`;
}
