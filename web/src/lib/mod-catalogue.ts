import {
	sourceLabel,
	type InstalledMod,
	type ModInstallTarget,
	type ModSource,
	type ModSummary,
	type RegistryStatus
} from '$lib/api/mods';

export function modOffer(mod: ModSummary, installed: InstalledMod | undefined) {
	if (!installed) return 'install';
	if (installed.source !== mod.source) return 'other-source';
	// A sync between fetches can change the listing; hide the update until both reads
	// agree on the version this row would install.
	return installed.update_version && installed.update_version === mod.latest_version
		? 'update'
		: 'installed';
}

export function installedUpdateTarget(mod: InstalledMod): ModInstallTarget | null {
	if (!mod.update_version) return null;
	return {
		full_name: mod.full_name,
		source: mod.source,
		name: mod.name || mod.full_name,
		latest_version: mod.update_version,
		is_deprecated: mod.is_deprecated
	};
}

export function catalogueStatus(
	registries: RegistryStatus[],
	selected: ModSource | null
): string | null {
	const rows = registries.filter((row) => selected === null || row.source === selected);
	if (selected && rows.some((row) => !row.enabled)) return `${sourceLabel[selected]} is disabled.`;
	const enabled = rows.filter((row) => row.enabled);
	const pending = enabled.filter((row) => !row.synced_at);
	if (pending.length === 0) return null;
	const names = pending.map((row) => sourceLabel[row.source]).join(' and ');
	return enabled.some((row) => row.synced_at)
		? `Partial catalogue. Waiting for ${names} to download.`
		: `Waiting for ${names} to download.`;
}
