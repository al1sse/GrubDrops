import { describe, expect, it } from 'vitest';
import type { ForceChannel } from '$lib/api/types';
import { movePlatformChannels, pointsWatchFlags, pointsWatchMode } from './points';

const channel = (platform: ForceChannel['platform'], name: string): ForceChannel => ({
	platform,
	channel: name,
	live: null,
	accounts: [],
	watching: []
});

describe('movePlatformChannels', () => {
	it('reorders Twitch ranks without moving Kick entries or changing the global slots', () => {
		const all = [channel('twitch', 'a'), channel('kick', 'k'), channel('twitch', 'b')];
		expect(movePlatformChannels(all, 'twitch', 0, 1).map(({ platform, channel: name }) => `${platform}:${name}`)).toEqual([
			'twitch:b', 'kick:k', 'twitch:a'
		]);
	});
});

describe('points watch mode', () => {
	it('maps all stored flag pairs to distinct, round-tripping modes', () => {
		const cases = [
			[true, false, 'drops_points'],
			[false, false, 'drops_only'],
			[true, true, 'points_only'],
			[false, true, 'none']
		] as const;
		for (const [enabled, only, mode] of cases) {
			expect(pointsWatchMode(enabled, only)).toBe(mode);
			expect(pointsWatchFlags(mode)).toEqual({ enabled, only });
		}
	});
});
