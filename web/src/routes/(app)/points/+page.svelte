<script lang="ts">
	import { onMount, tick } from 'svelte';
	import { api } from '$lib/app';
	import type { Account, ForceChannel, Platform, PointsSafety } from '$lib/api/types';
	import PageHead from '$lib/components/PageHead.svelte';
	import PlatformMark from '$lib/components/PlatformMark.svelte';
	import { formatInt } from '$lib/format';
	import { t } from '$lib/i18n/index.svelte';
	import { getLive } from '$lib/live-context';
	import { knownIds, parseChannel, toPut, togglePick, watchingIds } from '$lib/force-watch';
	import { MAX_POINTS_STREAMS, movePlatformChannels, pointsWatchFlags, pointsWatchMode, validStreams, type PointsWatchMode } from '$lib/points';
	import { reorder } from '$lib/drag';

	const live = getLive();
	type Tab = Platform | 'safety';
	let tab = $state<Tab>('twitch');
	type PointsValue = { enabled: boolean; only: boolean; streams: number };
	// Only while a save is in flight; otherwise the inputs show live.accounts.
	let accountPending = $state<Record<string, PointsValue>>({});
	let accountBusy = $state<Record<string, boolean>>({});
	let accountError = $state<Record<string, boolean>>({});
	let safety = $state<PointsSafety>({ sleep_enabled: true, sleep_start: '02:00', sleep_end: '08:00', follow_raids: true });
	let safetyState = $state<'loading' | 'ready' | 'failed'>('loading');
	let safetyBusy = $state(false);
	let safetyError = $state(false);
	let safetySave = $state<HTMLButtonElement | null>(null);
	let listSaving = $state(false);
	let listError = $state<Tab | null>(null);
	let drag = $state<{ key: string; to: number } | null>(null);
	let channelPanel = $state<HTMLElement | null>(null);
	let channelDialog = $state<HTMLDialogElement | null>(null);
	let channelForm = $state<string | null>(null);
	let channelUrl = $state('');
	let channelPlatform = $state<Platform | null>(null);
	let channelAccounts = $state<string[]>([]);
	let channelConfirm = $state<string | null>(null);

	const tabs = [{ id: 'twitch', label: 'accounts.twitch' }, { id: 'kick', label: 'accounts.kick' }, { id: 'safety', label: 'nav.settings' }] as const;
	// The list reloaded from the server (not our own write) supersedes a stale save error.
	let ownList: ForceChannel[] | null = null;
	function setList(list: ForceChannel[] | null): void {
		live.forceWatch = list;
		ownList = live.forceWatch;
	}

	function selectTab(id: Tab): void {
		tab = id;
		listError = null;
		channelConfirm = null;
	}

	function onTabKey(e: KeyboardEvent): void {
		const i = tabs.findIndex((item) => item.id === tab);
		const n = e.key === 'ArrowRight' ? (i + 1) % tabs.length : e.key === 'ArrowLeft' ? (i + tabs.length - 1) % tabs.length : e.key === 'Home' ? 0 : e.key === 'End' ? tabs.length - 1 : -1;
		if (n < 0) return;
		e.preventDefault();
		selectTab(tabs[n].id);
		void tick().then(() => document.getElementById(`points-tab-${tabs[n].id}`)?.focus());
	}
	const channels = $derived((live.forceWatch ?? []).filter((c) => c.platform === tab));
	const dragFrom = $derived(drag ? channels.findIndex((c) => channelKey(c) === drag?.key) : -1);
	const rows = $derived(drag && dragFrom >= 0 ? movePlatformChannels(live.forceWatch ?? [], tab as Platform, dragFrom, drag.to).filter((c) => c.platform === tab) : channels);
	const accountNames = $derived(new Map(live.accounts.map((a) => [a.id, a.name])));
	const knownAccounts = $derived(new Set(live.accounts.map((a) => a.id)));
	const parsedChannel = $derived(channelForm === 'new' ? parseChannel(channelUrl) : null);
	const editingChannel = $derived(channelForm && channelForm !== 'new' ? (live.forceWatch ?? []).find((c) => channelKey(c) === channelForm) : undefined);
	const channelDuplicate = $derived(!!parsedChannel && (live.forceWatch ?? []).some((c) => c.platform === parsedChannel.platform && c.channel === parsedChannel.name));
	const channelUrlError = $derived(channelForm === 'new' && channelUrl.trim() ? (!parsedChannel ? t('force.url_bad') : parsedChannel.platform !== channelPlatform ? t('force.platform_mismatch') : channelDuplicate ? t('force.dupe') : '') : '');
	const channelChoices = $derived(channelPlatform ? live.accounts.filter((a) => a.platform === channelPlatform) : []);
	const canSaveChannel = $derived(channelAccounts.length > 0 && (channelForm === 'new' ? !!parsedChannel && parsedChannel.platform === channelPlatform && !channelDuplicate : !!editingChannel));
	const listBusy = $derived(listSaving || live.forceWatch === null);

	$effect(() => {
		if (live.forceWatch !== ownList) listError = null;
	});
	// A reload that removed the channel being edited must not leave an 'Add channel' form behind.
	$effect(() => {
		if (channelForm && channelForm !== 'new' && !editingChannel) channelDialog?.close();
	});

	function channelKey(c: Pick<ForceChannel, 'platform' | 'channel'>): string {
		return `${c.platform}/${c.channel}`;
	}

	const channelTool = (key: string, name: string) => `li[data-k="${key}"] [data-t="${name}"]`;
	const channelPlus = '[data-t="plus"]';

	async function openChannelDialog(c: ForceChannel | null = null): Promise<void> {
		channelForm = c ? channelKey(c) : 'new';
		channelPlatform = c?.platform ?? (tab === 'safety' ? null : tab);
		channelUrl = '';
		channelAccounts = c ? knownIds(c.accounts, knownAccounts) : [];
		channelConfirm = null;
		await tick();
		channelDialog?.showModal();
	}

	function loadSafety(): void {
		safetyState = 'loading';
		void api.pointsSafety().then((value) => {
			safety = value;
			safetyState = 'ready';
		}).catch(() => {
			safetyState = 'failed';
		});
	}
	onMount(loadSafety);

	function accountValue(a: Account): PointsValue {
		return accountPending[a.id] ?? { enabled: a.points_enabled ?? true, only: a.points_only ?? false, streams: a.points_streams ?? 2 };
	}
	function saveMode(a: Account, mode: PointsWatchMode): void {
		void saveAccount(a, pointsWatchFlags(mode));
	}

	// Edits made while a save is in flight update accountPending; the loop sends the latest until it settles.
	async function saveAccount(a: Account, patch: Partial<PointsValue>): Promise<void> {
		accountPending[a.id] = { ...accountValue(a), ...patch };
		if (accountBusy[a.id]) return;
		accountError[a.id] = false;
		accountBusy[a.id] = true;
		try {
			for (;;) {
				const next = accountPending[a.id];
				await api.setAccountPoints(a.id, {
					points_enabled: next.enabled,
					points_only: next.only,
					...(a.platform === 'twitch' ? { points_streams: next.streams } : {})
				});
				// Merge into the current row, not the copy captured before the request.
				live.accounts = live.accounts.map((item) => item.id === a.id ? { ...item, points_enabled: next.enabled, points_only: next.only, ...(a.platform === 'twitch' ? { points_streams: next.streams } : {}) } : item);
				const latest = accountPending[a.id];
				if (latest.enabled === next.enabled && latest.only === next.only && latest.streams === next.streams) break;
			}
		} catch {
			accountError[a.id] = true;
		} finally {
			delete accountPending[a.id];
			accountBusy[a.id] = false;
		}
	}

	async function saveSafety(): Promise<void> {
		if (safetyBusy || safetyState !== 'ready' || safety.sleep_start === safety.sleep_end) return;
		safetyBusy = true;
		safetyError = false;
		try {
			safety = await api.savePointsSafety(safety);
		} catch {
			safetyError = true;
		} finally {
			safetyBusy = false;
			// A disabled button drops focus; hand it back once the button is enabled again.
			await tick();
			safetySave?.focus();
		}
	}

	async function refocus(targets: string[]): Promise<void> {
		await tick();
		const at = document.activeElement;
		if (at && at !== document.body && !channelPanel?.contains(at)) return;
		for (const selector of targets) {
			const el = channelPanel?.querySelector<HTMLButtonElement>(selector);
			if (el && !el.disabled) {
				el.focus();
				return;
			}
		}
	}

	async function commitChannels(next: ForceChannel[], focus: string[] = [channelPlus]): Promise<void> {
		if (listSaving || !live.forceWatch) return;
		listSaving = true;
		channelConfirm = null;
		listError = null;
		const startTab = tab;
		const previous = live.forceWatch;
		setList(next);
		try {
			setList((await api.putForceWatch(toPut(next, knownAccounts))).channels);
		} catch {
			setList(previous);
			try {
				setList((await api.forceWatch()).channels);
			} catch {
				/* the force_watch event refetches */
			}
			listError = startTab;
		} finally {
			listSaving = false;
		}
		await refocus(focus);
	}

	function saveChannel(): void {
		if (listBusy || !canSaveChannel || !channelPlatform) return;
		const current = live.forceWatch ?? [];
		const target = editingChannel;
		const parsed = parsedChannel;
		const selected = [...channelAccounts];
		const next = target
			? current.map((c) => channelKey(c) === channelKey(target) ? { ...c, accounts: selected } : c)
			: parsed ? [...current, { platform: parsed.platform, channel: parsed.name, live: null, accounts: selected, watching: [] }] : current;
		channelDialog?.close();
		void commitChannels(next, target ? [channelTool(channelKey(target), 'edit'), channelPlus] : [channelPlus]);
	}

	function move(from: number, to: number): void {
		const channel = channels[from];
		if (from === to || !live.forceWatch || !channel) return;
		const key = channelKey(channel);
		const first = to < from ? 'up' : 'down';
		const second = to < from ? 'down' : 'up';
		void commitChannels(movePlatformChannels(live.forceWatch, tab as Platform, from, to), [channelTool(key, first), channelTool(key, second), channelPlus]);
	}
	async function askRemoveChannel(c: ForceChannel): Promise<void> {
		const key = channelKey(c);
		channelConfirm = key;
		await tick();
		channelPanel?.querySelector<HTMLButtonElement>(channelTool(key, 'confirm'))?.focus();
	}

	async function cancelRemoveChannel(c: ForceChannel): Promise<void> {
		channelConfirm = null;
		await tick();
		channelPanel?.querySelector<HTMLButtonElement>(channelTool(channelKey(c), 'remove'))?.focus();
	}

	function removeChannel(c: ForceChannel): void {
		channelConfirm = null;
		void commitChannels((live.forceWatch ?? []).filter((item) => channelKey(item) !== channelKey(c)), [channelPlus]);
	}

	const dragOpts = {
		items: (list: HTMLElement) => Array.from(list.querySelectorAll<HTMLElement>(':scope > li')),
		enabled: () => !listBusy,
		onStart: (from: number) => {
			const c = channels[from];
			if (c) drag = { key: channelKey(c), to: from };
		},
		onMove: (to: number) => { if (drag) drag.to = to; },
		onEnd: (_from: number, to: number) => {
			const key = drag?.key;
			drag = null;
			const from = key ? channels.findIndex((c) => channelKey(c) === key) : -1;
			if (from >= 0) move(from, to);
		},
		onCancel: () => (drag = null)
	};

	function accountBalances(channel: ForceChannel): string[] {
		return (channel.points ?? []).map((point) => {
			const name = accountNames.get(point.account_id) ?? point.account_id;
			return point.balance === null ? t('points.balance_waiting', { name }) : t('points.balance', { name, balance: formatInt(point.balance) });
		});
	}
</script>

<PageHead title={t('nav.points')}>
	<div class="tabs" role="tablist" aria-label={t('points.tabs')}>
		{#each tabs as item (item.id)}
			<button type="button" role="tab" id={`points-tab-${item.id}`} aria-controls="points-panel" aria-selected={tab === item.id} tabindex={tab === item.id ? 0 : -1} class:on={tab === item.id} onclick={() => selectTab(item.id)} onkeydown={onTabKey}>{t(item.label)}</button>
		{/each}
	</div>

	<div role="tabpanel" id="points-panel" aria-labelledby={`points-tab-${tab}`}>
	{#if tab === 'safety'}
		<section class="panel" aria-labelledby="safety-h">
			<h2 id="safety-h">{t('points.safety')}</h2>
			{#if safetyState === 'loading'}<p>{t('points.loading')}</p>
			{:else if safetyState === 'failed'}
				<p class="bad" role="alert">{t('points.load_failed')}</p>
				<button type="button" class="save" onclick={loadSafety}>{t('inventory.retry')}</button>
			{:else}
				<label class="check"><input type="checkbox" bind:checked={safety.sleep_enabled} disabled={safetyBusy} />{t('points.sleep_enabled')}</label>
				<div class="times">
					<label>{t('points.sleep_start')}<input type="time" bind:value={safety.sleep_start} disabled={!safety.sleep_enabled || safetyBusy} /></label>
					<label>{t('points.sleep_end')}<input type="time" bind:value={safety.sleep_end} disabled={!safety.sleep_enabled || safetyBusy} /></label>
				</div>
				<label class="check"><input type="checkbox" bind:checked={safety.follow_raids} disabled={safetyBusy} />{t('points.follow_raids')}</label>
				{#if safety.sleep_start === safety.sleep_end}<p class="bad" role="alert">{t('points.sleep_times_equal')}</p>{/if}
				{#if safetyError}<p class="bad" role="alert">{t('points.save_failed')}</p>{/if}
				<button type="button" class="save primary" bind:this={safetySave} disabled={safetyBusy || safety.sleep_start === safety.sleep_end} onclick={saveSafety}>{safetyBusy ? t('points.saving') : t('points.save')}</button>
			{/if}
		</section>
	{:else}
		<section class="panel" aria-labelledby="accounts-h">
			<h2 id="accounts-h">{t('points.account_settings')}</h2>
			{#if live.accounts.filter((a) => a.platform === tab).length === 0}<p>{t('points.no_accounts')}</p>{/if}
			{#each live.accounts.filter((a) => a.platform === tab) as a (a.id)}
				{@const value = accountValue(a)}
				{@const mode = pointsWatchMode(value.enabled, value.only)}
				<div class="account" class:busy={accountBusy[a.id]}>
					<strong>{a.name}</strong>
					<label class="mode">{t('points.watch_mode')}
						<select value={mode} aria-busy={accountBusy[a.id]} onchange={(e) => saveMode(a, e.currentTarget.value as PointsWatchMode)}>
							{#if mode === 'none'}<option value="none">{t(a.platform === 'kick' ? 'points.mode_kick_none' : 'points.mode_none')}</option>{/if}
							<option value="drops_points">{t(a.platform === 'kick' ? 'points.mode_kick_drops_watch' : 'points.mode_drops_points')}</option>
							<option value="drops_only">{t('points.mode_drops_only')}</option>
							<option value="points_only">{t(a.platform === 'kick' ? 'points.mode_kick_watch_only' : 'points.mode_points_only')}</option>
						</select>
					</label>
					{#if a.platform === 'twitch'}
						<label class="stream">{t('points.streams')}<input type="number" min="1" max={MAX_POINTS_STREAMS} step="1" value={value.streams} aria-busy={accountBusy[a.id]} onchange={(e) => {
							const n = e.currentTarget.valueAsNumber;
							if (validStreams(n)) void saveAccount(a, { streams: n });
							else e.currentTarget.value = String(value.streams);
						}} /></label>
					{/if}
					{#if accountError[a.id]}<p class="bad" role="alert">{t('points.save_failed')}</p>{/if}
				</div>
			{/each}
		</section>
		<section class="panel" aria-labelledby="channels-h" bind:this={channelPanel}>
			<div class="channel-head">
				<h2 id="channels-h">{t('points.channels', { platform: t(tab === 'twitch' ? 'accounts.twitch' : 'accounts.kick') })}</h2>
				<button type="button" class="channel-add" data-t="plus" aria-label={t('force.add')} title={t('force.add')} disabled={listBusy} onclick={() => void openChannelDialog()}>+</button>
			</div>
			<p class="hint">{t('points.channel_hint')}</p>
			{#if listError === tab}<p class="bad" role="alert">{t('points.list_save_failed')}</p>{/if}
			{#if live.forceWatch === null}<p>{t('points.loading')}</p>
			{:else if rows.length === 0}<p>{t('points.no_channels')}</p>{:else}
				<ol use:reorder={dragOpts}>
					{#each rows as channel, i (`${channel.platform}/${channel.channel}`)}
						{@const channelId = channelKey(channel)}
						{@const active = watchingIds(channel, live.runners)}
						{@const assigned = channel.accounts.filter((id) => accountNames.has(id))}
						<li data-k={channelId} class:dragging={drag && drag.to === i}>
							<div class="row">
								<span class="rank">{i + 1}</span><PlatformMark platform={channel.platform} /><strong>{channel.channel}</strong>
								{#if channel.live !== null}<span class="dot" class:live={channel.live} role="img" aria-label={channel.live ? t('force.live') : t('force.offline')} title={channel.live ? t('force.live') : t('force.offline')}></span>{/if}
								<div class="tools" role="group" aria-label={channel.channel}>
									<button type="button" data-t="up" aria-label={t('points.move_up')} title={t('points.move_up')} disabled={i === 0 || listBusy} onclick={() => move(i, i - 1)}>↑</button>
									<button type="button" data-t="down" aria-label={t('points.move_down')} title={t('points.move_down')} disabled={i === rows.length - 1 || listBusy} onclick={() => move(i, i + 1)}>↓</button>
									<button type="button" data-t="edit" aria-label={t('force.edit')} title={t('force.edit')} disabled={listBusy} onclick={() => void openChannelDialog(channel)}>✎</button>
									<button type="button" data-t="remove" aria-label={t('force.remove')} title={t('force.remove')} disabled={listBusy} onclick={() => void askRemoveChannel(channel)}>×</button>
								</div>
							</div>
							{#if assigned.length}<small class="channel-accounts">{#each assigned as id, n (id)}{n ? ' · ' : ''}<span class:now={active.includes(id)}>{accountNames.get(id)}</span>{/each}</small>{/if}
							{#if channel.platform === 'twitch' && (channel.points?.length ?? 0) > 0}<small class="balances">{accountBalances(channel).join(' · ')}</small>{/if}
							{#if channelConfirm === channelId}
								<div class="confirm">
									<span>{t('force.remove_confirm', { name: channel.channel })}</span>
									<button type="button" class="btn danger" data-t="confirm" disabled={listBusy} onclick={() => removeChannel(channel)}>{t('force.remove')}</button>
									<button type="button" class="btn" onclick={() => void cancelRemoveChannel(channel)}>{t('force.cancel')}</button>
								</div>
							{/if}
						</li>
					{/each}
				</ol>
			{/if}
		</section>
	{/if}
	</div>
	<dialog class="channel-dialog" bind:this={channelDialog} aria-labelledby="channel-dialog-h" onclose={() => { channelForm = null; channelPlatform = null; }} onclick={(e) => e.target === channelDialog && channelDialog?.close()}>
		{#if channelForm}
			<div class="dialog-body">
				<h2 id="channel-dialog-h">{editingChannel ? t('force.edit_title', { name: editingChannel.channel }) : t('force.add')}</h2>
				{#if channelForm === 'new'}
					<label class="dialog-field">
						<span>{t('force.url_label')}</span>
						<!-- svelte-ignore a11y_autofocus -->
						<input type="url" placeholder={channelPlatform === 'twitch' ? 'https://www.twitch.tv/channel' : 'https://kick.com/channel'} bind:value={channelUrl} autofocus />
					</label>
					{#if channelUrlError}<p class="bad">{channelUrlError}</p>{/if}
					{#if parsedChannel && parsedChannel.platform === channelPlatform && !channelDuplicate}
						<p class="found"><PlatformMark platform={parsedChannel.platform} /><b>{parsedChannel.name}</b><span>{parsedChannel.platform === 'twitch' ? t('force.twitch_channel') : t('force.kick_channel')}</span></p>
					{/if}
				{/if}
				{#if channelPlatform}
					<fieldset>
						<legend>{t('force.accounts_label')}</legend>
						{#if channelChoices.length}
							<div class="chips">
								{#each channelChoices as a (a.id)}
									<button type="button" class="chip" class:on={channelAccounts.includes(a.id)} aria-pressed={channelAccounts.includes(a.id)} onclick={() => (channelAccounts = togglePick(channelAccounts, a.id))}><PlatformMark platform={a.platform} size="sm" />{a.name}</button>
								{/each}
							</div>
						{:else}
							<p class="bad">{channelPlatform === 'twitch' ? t('force.no_twitch') : t('force.no_kick')}</p>
						{/if}
					</fieldset>
				{/if}
				<div class="dialog-actions">
					<button type="button" class="btn" onclick={() => channelDialog?.close()}>{t('force.cancel')}</button>
					<button type="button" class="btn primary" disabled={!canSaveChannel || listBusy} onclick={saveChannel}>{editingChannel ? t('force.save') : t('force.add_btn')}</button>
				</div>
			</div>
		{/if}
	</dialog>
</PageHead>

<style>
	.tabs{display:flex;gap:8px;border-bottom:1px solid var(--border);margin-bottom:18px}.tabs button{font:inherit;color:var(--muted);background:transparent;border:0;border-bottom:2px solid transparent;padding:10px 14px;cursor:pointer}.tabs button.on{color:var(--text);border-color:var(--accent)}
	.panel{background:var(--surface);border:1px solid var(--border);border-radius:12px;padding:18px;margin-bottom:16px}.panel h2{font-size:17px;margin:0 0 14px}.account{display:flex;align-items:center;gap:18px;flex-wrap:wrap;border-top:1px solid var(--border);padding:12px 0}.account:first-of-type{border-top:0}.account strong{min-width:120px}.check{display:flex;align-items:center;gap:8px}.check input{accent-color:var(--accent)}.stream,.times label{display:grid;gap:5px;color:var(--muted);font-size:13px}.stream input,.times input{font:inherit;color:var(--text);background:var(--bg);border:1px solid var(--border);border-radius:7px;padding:7px}.stream input{width:110px}.times{display:flex;gap:14px;margin:14px 0}.times input{width:145px}.hint{color:var(--muted);font-size:13px}ol{list-style:none;padding:0;margin:0}li{border-top:1px solid var(--border);padding:10px 0}.row{display:flex;align-items:center;gap:10px}.rank{width:24px;color:var(--muted)}.row strong{flex:1}.tools{display:flex;gap:5px}.tools button{width:34px;height:34px;border:1px solid var(--border);border-radius:7px;background:var(--bg);color:var(--text);cursor:pointer}.tools button:disabled{opacity:.45;cursor:default}.balances{display:block;margin:4px 0 0 34px;color:var(--muted)}.dragging{opacity:.5}.bad{color:var(--red)}.save{margin-top:14px}.busy{opacity:.7}
	.mode{display:grid;gap:5px;color:var(--muted);font-size:13px}.mode select{font:inherit;color:var(--text);background:var(--bg);border:1px solid var(--border);border-radius:7px;padding:7px;max-width:220px}
	@media(max-width:600px){.account{gap:12px}.account strong{width:100%}.times{flex-direction:column}}
	.channel-head{display:flex;align-items:center;justify-content:space-between;gap:12px;margin-bottom:14px}.channel-head h2{margin:0}
	.channel-add{width:34px;height:34px;border:1px solid var(--border);border-radius:7px;background:var(--bg);color:var(--text);font-size:22px;cursor:pointer}.channel-add:disabled{opacity:.45;cursor:default}
	.channel-accounts{display:block;margin:4px 0 0 34px;color:var(--muted)}.channel-accounts .now{color:var(--green);font-weight:600}
	.dot{display:inline-block;flex:none;width:7px;height:7px;margin-left:8px;border-radius:50%;background:var(--muted)}.dot.live{background:var(--red)}
	.confirm{display:flex;align-items:center;gap:8px;flex-wrap:wrap;padding:8px 0 0 34px}.confirm .btn.danger{border-color:var(--red);color:var(--red)}
	.channel-dialog{width:min(420px,calc(100vw - 32px));max-height:calc(100dvh - 32px);padding:18px;border:1px solid var(--border);border-radius:12px;background:var(--surface);color:var(--text)}
	.channel-dialog::backdrop{background:rgb(0 0 0 / .55)}.dialog-body{display:grid;gap:10px}.dialog-body h2{margin:0;font-size:16px}.dialog-body .bad{margin:0}
	.dialog-field{display:grid;gap:4px;color:var(--muted);font-size:12px}.channel-dialog input{min-width:0;padding:6px 8px;border:1px solid var(--border);border-radius:6px;background:var(--bg);color:var(--text);font:inherit;font-size:13px}.channel-dialog input:focus-visible{outline:2px solid var(--accent)}
	.found{display:flex;align-items:center;gap:6px;margin:0;font-size:13px}.found span{color:var(--muted);font-size:12px}
	.channel-dialog fieldset{display:grid;gap:6px;margin:0;padding:0;border:0}.channel-dialog legend{margin-bottom:6px;padding:0;color:var(--muted);font-size:12px}
	.chips{display:flex;flex-wrap:wrap;gap:6px}.chip{display:inline-flex;align-items:center;gap:5px;padding:3px 9px;border:1px solid var(--border);border-radius:999px;background:var(--surface);color:var(--muted);font:inherit;font-size:12px;cursor:pointer}.chip.on{border-color:var(--accent);background:var(--accent-soft);color:var(--text)}.chip:focus-visible,.dialog-actions .btn:focus-visible,.confirm button:focus-visible,.save:focus-visible{outline:2px solid var(--accent)}
	.dialog-actions{display:flex;justify-content:flex-end;gap:6px}.dialog-actions .btn,.confirm .btn,.save{padding:4px 10px;border:1px solid var(--border);border-radius:6px;background:var(--surface);color:var(--text);font:inherit;font-size:12px;cursor:pointer}.dialog-actions .btn.primary,.save.primary{border-color:var(--accent-strong);background:var(--accent-strong);color:#fff}.dialog-actions .btn:disabled,.confirm .btn:disabled,.save:disabled{opacity:.45;cursor:default}
</style>
