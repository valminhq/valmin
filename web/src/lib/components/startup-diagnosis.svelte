<script lang="ts">
	import { resolve } from '$app/paths';
	import { instances, type Finding } from '#lib/api/instances.js';
	import * as Alert from '#lib/components/ui/alert/index.js';
	import { Button } from '#lib/components/ui/button/index.js';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import Info from '@lucide/svelte/icons/info';

	let { instanceId, serverState }: { instanceId: string; serverState: string } = $props();

	let findings = $state<Finding[]>([]);

	// Derived so a replaced row with the same id and state does not refetch.
	const id = $derived(instanceId);
	const phase = $derived(serverState);

	// Refetched on every state change, since a start that fails or recovers changes the answer.
	$effect(() => {
		void phase;
		let current = true;
		instances
			.diagnosis(id)
			.then((d) => {
				if (current) findings = d.findings;
			})
			.catch(() => {
				// The console stays usable without an explanation.
				if (current) findings = [];
			});
		return () => (current = false);
	});

	const targets = {
		mods: { label: 'Open mods', route: '/instances/[id]/mods' },
		settings: { label: 'Server settings', route: '/instances/[id]/settings' }
	} as const;
</script>

{#if findings.length > 0}
	<section aria-label="Start problems" class="grid gap-2">
		{#each findings as finding, i (i)}
			<Alert.Root variant={finding.severity === 'error' ? 'destructive' : 'default'}>
				{#if finding.severity === 'error'}<TriangleAlert />{:else}<Info />{/if}
				<Alert.Title>{finding.title}</Alert.Title>
				<Alert.Description class="grid justify-items-start gap-2">
					<span>{finding.detail}</span>
					{#if finding.evidence}
						<code
							class="w-full rounded bg-muted px-2 py-1 font-mono text-xs break-words text-foreground"
						>
							{finding.evidence}
						</code>
					{/if}
					<ol class="list-decimal pl-5">
						{#each finding.fixes as fix (fix)}
							<li>{fix}</li>
						{/each}
					</ol>
					{#if finding.related?.length}
						<details>
							<summary class="cursor-pointer">
								{finding.related.length} more skipped because of this
							</summary>
							<ul class="list-disc pl-5">
								{#each finding.related as name (name)}
									<li>{name}</li>
								{/each}
							</ul>
						</details>
					{/if}
					{#if finding.target}
						{@const target = targets[finding.target]}
						<Button variant="outline" size="sm" href={resolve(target.route, { id: instanceId })}>
							{target.label}
						</Button>
					{/if}
				</Alert.Description>
			</Alert.Root>
		{/each}
	</section>
{/if}
