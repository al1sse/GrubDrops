import type { ForceChannel, Platform } from '$lib/api/types';

/** Reorders one platform's rows in place among their existing global-order slots. */
export function movePlatformChannels(
	channels: readonly ForceChannel[],
	platform: Platform,
	from: number,
	to: number
): ForceChannel[] {
	const selected = channels.filter((channel) => channel.platform === platform);
	if (from < 0 || from >= selected.length || to < 0 || to >= selected.length || from === to) return [...channels];
	const [moved] = selected.splice(from, 1);
	selected.splice(to, 0, moved);
	let index = 0;
	return channels.map((channel) => channel.platform === platform ? selected[index++] : channel);
}

/** Mirrors store.MaxPointsStreams. */
export const MAX_POINTS_STREAMS = 3;

/** Twitch accounts the UI allows; keep in step with accounts.twitch_limit. */
export const MAX_TWITCH_ACCOUNTS = 3;

export function validStreams(n: number): boolean {
	return Number.isInteger(n) && n >= 1 && n <= MAX_POINTS_STREAMS;
}

export type PointsWatchMode = 'drops_points' | 'drops_only' | 'points_only' | 'none';

export function pointsWatchMode(enabled: boolean, only: boolean): PointsWatchMode {
	return enabled ? (only ? 'points_only' : 'drops_points') : (only ? 'none' : 'drops_only');
}

export function pointsWatchFlags(mode: PointsWatchMode): { enabled: boolean; only: boolean } {
	switch (mode) {
		case 'drops_points': return { enabled: true, only: false };
		case 'drops_only': return { enabled: false, only: false };
		case 'points_only': return { enabled: true, only: true };
		case 'none': return { enabled: false, only: true };
	}
}
