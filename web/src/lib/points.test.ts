import { describe, expect, it } from 'vitest';
import type { ForceChannel } from '$lib/api/types';
import { MAX_POINTS_STREAMS, movePlatformChannels, pointsWatchFlags, pointsWatchMode, validStreams } from './points';

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
	it('moves a Kick row past interleaved Twitch rows and leaves every other row in place', () => {
		const all = [channel('kick', 'k1'), channel('twitch', 'a'), channel('kick', 'k2'), channel('twitch', 'b'), channel('kick', 'k3')];
		expect(movePlatformChannels(all, 'kick', 0, 2).map((c) => c.channel)).toEqual(['k2', 'a', 'k3', 'b', 'k1']);
	});
	it('returns an unchanged copy for out-of-range or identical indexes', () => {
		const all = [channel('twitch', 'a'), channel('kick', 'k'), channel('twitch', 'b')];
		for (const [from, to] of [[0, 0], [-1, 1], [0, 2], [2, 0]]) {
			const out = movePlatformChannels(all, 'twitch', from, to);
			expect(out).not.toBe(all);
			expect(out).toEqual(all);
		}
	});
});

describe('validStreams', () => {
	it('accepts integers from 1 to the cap only', () => {
		expect(MAX_POINTS_STREAMS).toBe(10);
		expect([1, 2, 10].every(validStreams)).toBe(true);
		expect([0, 11, 100, 1.5, NaN, -1].some(validStreams)).toBe(false);
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
