<script lang="ts">
	import { playerHistory, type PlayerObservation } from '$lib/api/players';
	import { socketStatus } from '$lib/socket/index.svelte';
	import * as Card from '$lib/components/ui/card';
	import Problem from '$lib/components/problem.svelte';

	let { instanceId }: { instanceId: string } = $props();

	let points = $state<PlayerObservation[]>([]);
	let failure = $state<unknown>(null);
	let loaded = $state(false);

	// The daemon writes history whether or not anyone is watching, so a socket that dropped
	// and came back has a gap to fill: refetch rather than splice.
	$effect(() => {
		void socketStatus.value;
		void load();
	});

	async function load() {
		try {
			points = (await playerHistory.list(instanceId)).items;
			failure = null;
		} catch (err) {
			failure = err;
		} finally {
			loaded = true;
		}
	}

	interface Span {
		from: number;
		to: number;
		players: number | null;
	}

	// Each observation holds until the next one, so a point becomes the span that follows it.
	// The final span runs to now: the count has not changed since, which is the whole reason
	// nothing was written.
	const spans = $derived.by((): Span[] => {
		const ordered = [...points].reverse();
		const now = Date.now();
		return ordered
			.map((p, i) => ({
				from: Date.parse(p.observed_at),
				to: i + 1 < ordered.length ? Date.parse(ordered[i + 1].observed_at) : now,
				players: p.players
			}))
			.filter((s) => Number.isFinite(s.from) && s.to > s.from);
	});

	const window = $derived(spans.length === 0 ? 0 : spans[spans.length - 1].to - spans[0].from);
	// Nulls are excluded rather than counted as zero: a gap has no height, and folding it in
	// as a floor would be the panel inventing a reading.
	const peak = $derived(
		Math.max(1, ...spans.filter((s) => s.players !== null).map((s) => s.players as number))
	);

	function time(ms: number): string {
		return new Date(ms).toLocaleString();
	}

	function label(s: Span): string {
		const when = `${time(s.from)} to ${time(s.to)}`;
		if (s.players === null) return `${when}: not observed`;
		return `${when}: ${s.players} ${s.players === 1 ? 'player' : 'players'}`;
	}
</script>

<Card.Root>
	<Card.Header>
		<Card.Title>Observed players</Card.Title>
		<Card.Description>
			What the server reported while the panel was reading its log. A stretch marked “not observed”
			is a gap in the panel's watching, not an empty server.
		</Card.Description>
	</Card.Header>
	<Card.Content class="grid gap-4">
		<Problem error={failure} />

		{#if loaded && spans.length === 0}
			<p class="text-sm text-muted-foreground">
				Nothing observed yet. History starts the first time the panel reads a running server's log.
			</p>
		{:else if spans.length > 0}
			<!-- Decorative: every span below is in the table, which is what a screen reader and a
			     keyboard reach. -->
			<div class="flex h-6 overflow-hidden rounded border" aria-hidden="true">
				{#each spans as span (span.from)}
					<div
						class="h-full {span.players === null
							? 'bg-muted'
							: span.players === 0
								? 'bg-secondary'
								: 'bg-primary'}"
						style="width: {((span.to - span.from) / window) * 100}%; opacity: {span.players
							? 0.4 + (0.6 * span.players) / peak
							: 1}"
						title={label(span)}
					></div>
				{/each}
			</div>

			<div class="overflow-x-auto">
				<table class="w-full text-sm">
					<caption class="sr-only">
						Observed player counts, newest first. Each row holds until the next.
					</caption>
					<thead>
						<tr class="text-left text-xs text-muted-foreground">
							<th scope="col" class="pb-1 font-normal">From</th>
							<th scope="col" class="pb-1 font-normal">To</th>
							<th scope="col" class="pb-1 font-normal">Players</th>
						</tr>
					</thead>
					<tbody>
						{#each [...spans].reverse() as span (span.from)}
							<tr class="border-t">
								<td class="py-1 tabular-nums">{time(span.from)}</td>
								<td class="py-1 tabular-nums">{time(span.to)}</td>
								<td class="py-1">
									{#if span.players === null}
										<span class="text-muted-foreground">not observed</span>
									{:else}
										<span class="tabular-nums">{span.players}</span>
									{/if}
								</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		{/if}
	</Card.Content>
</Card.Root>
