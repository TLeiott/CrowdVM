// Sandbox detail: header, actions, tabs. Tab bodies live in js/tabs/*.js.
import { h, icon, btn, time, every, patch, ago, duration, num, bytes } from '../dom.js';
import { api } from '../api.js';
import { pageHead, card, stateChip, menuButton, loading, errorBox, tabs, copyable, idText, isLive, isStopped, isBusy } from '../ui.js';
import { sandboxMenu, stopSandbox, resumeDialog, forkDialog, extendDialog, sizeText, nodeNames, nodeOf, sbAccess, CREATOR_ONLY, shareLabels } from '../sbx.js';

const TABS = [
  ['', 'Overview', 'activity'],
  ['console', 'Console', 'terminal'],
  ['files', 'Files', 'folder'],
  ['agent', 'Agent', 'bot'],
  ['desktop', 'Desktop', 'monitor'],
  ['metrics', 'Metrics', 'chart'],
  ['snapshots', 'Snapshots', 'camera'],
  ['ports', 'Public URLs', 'globe'],
];

export default async function (ctx) {
  const id = ctx.params.id, tab = ctx.params.tab || '';
  const base = `/sandboxes/${id}`;
  if (!TABS.some(([k]) => k === tab)) return ctx.nav(base, { replace: true });
  let sb = null;

  const head = h('div');
  const body = h('div', loading());
  ctx.root.append(head, tabs(TABS, tab, base), body);
  ctx.wide(tab === 'desktop');

  async function load() {
    const r = await api.get(`/sandboxes/${id}`, undefined, { signal: ctx.signal });
    sb = r.sandbox || r;
    ctx.title(sb.name);
    drawHead();
    return sb;
  }

  function drawHead() {
    patch(head, sb, () => {
      const live = isLive(sb.state), stopped = isStopped(sb.state), acc = sbAccess(sb);
      const primary = [];
      if (live && acc !== 'view') primary.push(h('a.btn', { href: `${base}/desktop` }, icon('monitor'), h('span.hide-sm', 'Desktop')));
      if (live || isBusy(sb.state)) primary.push(btn('Stop', { ico: 'stop', onClick: () => stopSandbox(sb).then((ok) => ok && load()) }));
      if ((stopped || sb.state === 'error') && acc !== 'view') primary.push(btn('Resume', { kind: 'primary', ico: 'play', onClick: () => resumeDialog(sb).then((ok) => ok && load()) }));
      primary.push(btn('', { ico: 'fork', title: acc === 'owner' ? 'Fork' : `Fork: ${CREATOR_ONLY}`, disabled: acc !== 'owner', onClick: () => forkDialog(ctx, sb) }));
      primary.push(menuButton(() => sandboxMenu(ctx, sb, (deleted) => deleted === true ? ctx.nav('/sandboxes') : load()), { kind: '', size: '', title: 'More actions' }));
      return [
        pageHead({
          crumbs: [{ label: 'Sandboxes', href: '/sandboxes' }, { label: sb.id }],
          title: h('span.row.wrap', { style: { gap: '10px' } }, sb.name || sb.id, stateChip(sb.state), shareLabels(sb),
            acc !== 'owner' && sb.createdBy ? h('span.faint.small', { style: { fontWeight: 400 } }, `by ${sb.createdBy}`) : null,
            sb.health === 'degraded' ? h('span.chip.warn', { title: sb.healthReason || '' }, 'degraded') : null),
          actions: primary,
        }),
        sb.error && (sb.state === 'error' || stopped) ? h('div.notice.err', { style: { marginBottom: '12px' } }, icon('alert'), h('div', h('b', sb.state === 'error' ? 'Error: ' : 'Stopped: '), sb.error)) : null,
        sb.health === 'degraded' ? h('div.notice.warn', { style: { marginBottom: '12px' } }, icon('alert'), h('div', h('b', 'Degraded. '), sb.healthReason || 'The server cannot fully reach this machine. It keeps running.', sb.degradedSince ? ` Since ${ago(sb.degradedSince)}.` : '')) : null,
        sb.hydrated === false && sb.hydration ? h('div.notice', { style: { marginBottom: '12px' } }, h('span.spinner'), h('div', 'Restoring files from the snapshot', sb.hydration.percent != null ? ` (${sb.hydration.percent}%)` : '', '. The sandbox is already usable.')) : null,
      ];
    });
  }

  try { await load(); } catch (e) {
    if (e.name === 'AbortError') return;
    body.replaceChildren(errorBox(e.status === 404 ? new Error(`Sandbox ${id} does not exist, or you cannot see it.`) : e));
    return;
  }
  const sbListeners = [];
  every(tab === '' ? 3000 : 6000, () => load().then(() => sbListeners.forEach((f) => f(sb))), ctx.signal);
  const tctx = { ...ctx, id, get sb() { return sb; }, reload: load, onSandbox: (f) => sbListeners.push(f) };

  if (tab === '') return overview(tctx, body);
  const mod = await import(`../tabs/${tab}.js`);
  if (ctx.signal.aborted) return;
  body.replaceChildren();
  await mod.default(tctx, body);
}

// ---------------- overview ----------------
async function overview(ctx, body) {
  const sb = ctx.sb;
  const details = h('div'), connect = h('div'), side = h('div.stack');
  body.replaceChildren(h('div.grid.side', h('div.stack', details, connect), side));

  const names = await nodeNames();
  const drawDetails = (sb) => patch(details, sb, () => card({
    title: 'Details',
    actions: isLive(sb.state) ? btn('Auto-stop', { size: 'sm', ico: 'clock', onClick: () => extendDialog(sb).then((ok) => ok && ctx.reload()) }) : null,
    body: h('dl.kv',
      h('dt', 'ID'), h('dd', idText(sb.id)),
      h('dt', 'State'), h('dd', stateChip(sb.state), sb.stop?.status === 'failing' ? h('span.small.faint', `stop retrying: ${sb.stop.error}`) : null),
      h('dt', 'Size'), h('dd', h('span', { style: { textTransform: 'capitalize' } }, sb.type || '—'), h('span.faint', `· ${sizeText(sb)} · ${sb.billingMultiplier ?? '?'}× credits`)),
      h('dt', 'Node'), h('dd', nodeOf(sb, names) && !isStopped(sb.state) ? [icon('server', 'sm'), h('a', { href: '/nodes' }, nodeOf(sb, names).name), nodeOf(sb, names).tier ? h('span.tier', { class: nodeOf(sb, names).tier }, nodeOf(sb, names).tier) : null] : h('span.faint', isStopped(sb.state) ? 'None while stopped. Resume picks a node.' : '—')),
      h('dt', 'Environment'), h('dd', sb.environment ? [h('a', { href: '/environments' }, sb.environment), sb.environmentVersion ? h('span.faint', `version ${sb.environmentVersion}`) : null] : h('span.faint', 'none (nothing passed in)')),
      h('dt', 'Auto-stop'), h('dd', sb.archiveAfter && isLive(sb.state) ? [h('span', ago(sb.archiveAfter)), h('span.faint', `(${duration(sb.ttlSeconds)} after start or resume)`)] : h('span.faint', sb.ttlSeconds == null ? 'off' : duration(sb.ttlSeconds))),
      h('dt', 'Last snapshot'), h('dd', sb.snapshotCompletedAt ? time(sb.snapshotCompletedAt) : h('span.faint', 'none yet'),
        sb.lastSnapshotStatus && sb.lastSnapshotStatus !== 'completed' ? stateChip(sb.lastSnapshotStatus === 'in_progress' ? 'saving' : sb.lastSnapshotStatus) : null),
      sb.setupStatus ? [h('dt', 'Setup script'), h('dd', stateChip(sb.setupStatus), sb.setupError ? h('code.small', sb.setupError) : null)] : null,
      h('dt', 'Created'), h('dd', time(sb.createdAt), sb.createdBy ? h('span.faint', `by ${sb.createdBy}`) : null),
      sb.visibility ? [h('dt', 'Visibility'), h('dd', h('span.chip.plain', { class: sb.visibility === 'shared' ? 'accent' : '' }, sb.visibility), sb.access && sb.access !== 'owner' ? h('span.faint', `you can ${sb.access}`) : null)] : null,
      sb.labels && Object.keys(sb.labels).length ? [h('dt', 'Labels'), h('dd', Object.entries(sb.labels).map(([k, v]) => h('span.tag', `${k}=${v}`)))] : null,
      sb.lastReboot ? [h('dt', 'Last reboot'), h('dd', time(sb.lastReboot.at), h('span.faint', sb.lastReboot.reason))] : null,
    ),
  }));
  drawDetails(sb);
  ctx.onSandbox(drawDetails);

  const origin = location.origin;
  connect.append(card({
    title: 'Connect', sub: 'From your machine, with the crowdvm CLI signed in to this server.',
    body: h('div.stack',
      h('div.field', h('span.label', 'SSH'), copyable(`crowdvm ssh ${sb.id}`)),
      h('div.field', h('span.label', 'Run a command'), copyable(`crowdvm exec ${sb.id} -- uname -a`)),
      h('div.field', h('span.label', 'Use the API'), copyable(`curl -H "Authorization: Bearer $CROWDVM_API_KEY" ${origin}/api/v1/sandboxes/${sb.id}`)),
      sb.sshEndpoint ? h('div.field', h('span.label', 'Direct SSH on the LAN'), copyable(`ssh -p ${sb.sshEndpoint.split(':').pop()} user@${sb.sshEndpoint.split(':')[0]}`)) : null),
  }));

  // side column: usage, processes, recent commands
  const usage = h('div'), procs = h('div'), cmds = h('div');
  side.append(usage, procs, cmds);
  // A teammate's private sandbox (view access) answers 403 for processes and history: do not ask.
  const priv = sbAccess(sb) === 'view';
  const loadSide = async () => {
    const [u, p, c] = await Promise.allSettled([
      api.get(`/sandboxes/${sb.id}/usage`),
      isLive(ctx.sb.state) && !priv ? api.get(`/sandboxes/${sb.id}/processes`) : Promise.reject(new Error(priv ? 'private' : 'stopped')),
      priv ? Promise.reject(new Error('private')) : api.get(`/sandboxes/${sb.id}/command-history`, { limit: 6 }),
    ]);
    patch(usage, u.value || null, (x) => card({
      title: 'Usage', sub: 'Since creation',
      body: x ? h('div.grid.cols-2', { style: { gap: '8px' } },
        h('div.stat', { style: { padding: 0 } }, h('span.k', 'Machine time'), h('span.v', duration(x.seconds / (x.billingMultiplier || 1)))),
        h('div.stat', { style: { padding: 0 } }, h('span.k', 'Credits'), h('span.v', num(x.credits ?? x.seconds / 3600, 2)), h('span.d', `${x.billingMultiplier}× rate${x.running ? ' · metering' : ''}`)))
        : h('p.faint', 'No usage data.'),
    }));
    patch(procs, p.value || (p.reason?.message === 'stopped' ? 'stopped' : null), (x) => card({
      title: 'Top processes', sub: x?.live === false ? `Last sample ${ago(x.sampledAt)}` : x?.total ? `${x.total} total` : null, flush: true,
      body: x === 'stopped' ? h('div.card-body.faint', 'Not running.') : !x ? h('div.card-body.faint', 'Unavailable.') :
        h('div.table-wrap', h('table.t', h('thead', h('tr', h('th', 'Command'), h('th.num', 'CPU'), h('th.num', 'Mem'))),
          h('tbody', (x.processes || []).slice(0, 6).map((pr) => h('tr', h('td', h('code.truncate', { style: { display: 'block', maxWidth: '140px' }, title: pr.command }, pr.command)),
            h('td.num', `${num(pr.cpuPercent, 1)}%`), h('td.num', bytes((pr.memoryMB || 0) * 1048576, 0))))))),
    }));
    patch(cmds, c.value?.commands || null, (list) => card({
      title: 'Recent commands', flush: true, actions: h('a.btn.sm.ghost', { href: `/sandboxes/${sb.id}/console` }, 'Console', icon('chevron', 'sm')),
      body: priv ? h('div.card-body.faint', 'Private to its creator.') : !list?.length ? h('div.card-body.faint', 'No commands yet.') : h('div.table-wrap', h('table.t', h('tbody', list.map((cm) => h('tr',
        h('td', h('code.truncate', { style: { display: 'block', maxWidth: '190px' }, title: cm.command }, cm.command), h('div.faint.small', ago(cm.startedAt))),
        h('td.act', cm.status === 'exited' ? h('span.chip', { class: cm.exitCode === 0 ? 'ok' : 'err' }, `exit ${cm.exitCode}`) : stateChip(cm.status === 'running' ? 'running' : cm.status, cm.status)))))))
    }));
  };
  loadSide().catch(() => {});
  every(5000, loadSide, ctx.signal);
}
