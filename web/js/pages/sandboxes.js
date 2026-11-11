// Sandboxes list.
import { h, icon, btn, time, every, patch, ago, num } from '../dom.js';
import { api } from '../api.js';
import { pageHead, card, table, stateChip, menuButton, empty, loading, errorBox, segmented, copyable, isStopped, isLive, toast, toastErr } from '../ui.js';
import { createSandboxDialog, sandboxMenu, sizeText, nodeNames, nodeOf, shareLabels, sbAccess } from '../sbx.js';

const FILTERS = [['all', 'All'], ['live', 'Running'], ['stopped', 'Stopped']];

export default async function (ctx) {
  ctx.title('Sandboxes');
  let filter = ctx.query.get('filter') || 'all', type = '', q = '', data = null, err = null, names = {};

  const search = h('input.input', { type: 'search', placeholder: 'Filter by name, id, label…', 'aria-label': 'Filter sandboxes', 'data-search': '' });
  search.addEventListener('input', () => { q = search.value.trim().toLowerCase(); draw(); });
  const typeSel = h('select.select', { 'aria-label': 'Size filter', style: { width: 'auto' }, onchange: (e) => { type = e.target.value; draw(); } },
    [['', 'All sizes'], ['small', 'Small'], ['default', 'Default'], ['large', 'Large'], ['xlarge', 'XLarge']].map(([v, l]) => h('option', { value: v }, l)));
  const createBtn = btn('New sandbox', { kind: 'primary', ico: 'plus', onClick: () => createSandboxDialog(ctx), attrs: { 'data-create': '' } });
  createBtn.append(h('span.kbd.hide-sm', { style: { background: 'transparent', color: 'inherit', borderColor: 'currentColor', opacity: '.6' } }, 'C'));
  const banner = h('div');
  const body = h('div', loading());

  ctx.root.append(
    pageHead({ title: 'Sandboxes', lede: 'Persistent Ubuntu machines on your crowd’s nodes. Stop one and its disk is kept; resume it on any node.', actions: createBtn }),
    banner,
    h('div.row.wrap', { style: { marginBottom: '12px', gap: '10px' } },
      h('div.search.grow', { style: { minWidth: '200px' } }, icon('search'), search, h('span.kbd.hide-sm', '/')),
      segmented(FILTERS, filter, (v) => { filter = v; history.replaceState(null, '', v === 'all' ? '/sandboxes' : `/sandboxes?filter=${v}`); draw(); }, 'State filter'),
      typeSel),
    body);

  const match = (sb) => {
    if (filter === 'live' && isStopped(sb.state)) return false;
    if (filter === 'stopped' && !isStopped(sb.state)) return false;
    if (type && (sb.type || 'default') !== type) return false;
    if (!q) return true;
    return [sb.name, sb.id, sb.environment, nodeOf(sb, names)?.name, sb.createdBy, ...Object.entries(sb.labels || {}).map(([k, v]) => `${k}=${v}`)]
      .some((s) => s && String(s).toLowerCase().includes(q));
  };

  function draw() {
    if (err && !data) return body.replaceChildren(errorBox(err, load));
    if (!data) return;
    const rows = data.filter(match);
    patch(body, { rows, filter, type, q, n: data.length, names: Object.keys(names).length }, () => {
      if (!data.length) {
        return card({ body: empty({
          ico: 'box', title: 'No sandboxes yet',
          text: 'Create one here, or from a terminal with the CLI. It boots on a node that has room.',
          action: h('div.stack', { style: { alignItems: 'center' } }, btn('New sandbox', { kind: 'primary', ico: 'plus', onClick: () => createSandboxDialog(ctx) }),
            h('div', { style: { width: 'min(320px, 80vw)' } }, copyable('crowdvm create --ttl 1h'))),
        }) });
      }
      return h('div.card', table({
        rows,
        onRow: (sb) => ctx.nav(`/sandboxes/${sb.id}`),
        rowLabel: (sb) => `${sb.name}, ${sb.state}`,
        emptyEl: empty({ ico: 'search', title: 'No matches', text: 'No sandbox matches this filter.' }),
        cols: [
          { label: 'Name', render: (sb) => h('div.stack.tight', { style: { gap: '1px' } },
            h('a.primary-cell', { href: `/sandboxes/${sb.id}` }, sb.name || sb.id),
            h('span.sub', sb.id, sbAccess(sb) !== 'owner' && sb.createdBy ? h('span', ` · by ${sb.createdBy}`) : null, sb.labels && Object.keys(sb.labels).length ? h('span', { style: { marginLeft: '6px' } }, Object.entries(sb.labels).slice(0, 2).map(([k, v]) => h('span.tag', { style: { marginRight: '4px' } }, `${k}=${v}`))) : null)) },
          { label: 'State', render: (sb) => h('div.row', stateChip(sb.state), sb.health === 'degraded' ? h('span.chip.warn', { title: sb.healthReason || '' }, 'degraded') : null,
            shareLabels(sb)) },
          { label: 'Size', cls: 'hide-xs nowrap', render: (sb) => h('div.stack.tight', { style: { gap: '0' } }, h('span', { style: { textTransform: 'capitalize' } }, sb.type || '—'), h('span.faint.small', sizeText(sb))) },
          { label: 'Node', cls: 'hide-md', render: (sb) => nodeOf(sb, names) && !isStopped(sb.state) ? h('span.row', { style: { gap: '6px' } }, icon('server', 'sm'), h('span.truncate', nodeOf(sb, names).name)) : h('span.faint', isStopped(sb.state) ? 'parked' : '—') },
          { label: 'Environment', cls: 'hide-md', render: (sb) => sb.environment ? h('span', sb.environment, sb.environmentVersion ? h('span.faint', ` v${sb.environmentVersion}`) : null) : h('span.faint', 'none') },
          { label: 'Auto-stop', cls: 'hide-xs', render: (sb) => isLive(sb.state) ? (sb.archiveAfter ? h('span', { title: new Date(sb.archiveAfter).toLocaleString() }, ago(sb.archiveAfter)) : h('span.faint', 'off')) : isStopped(sb.state) ? h('span.faint', sb.stoppedAt ? `stopped ${ago(sb.stoppedAt)}` : 'stopped') : h('span.faint', '—') },
          { label: h('span', { title: 'Credits spent this month' }, 'Cost'), cls: 'num hide-md', render: (sb) => sb.cost ? h('span', { title: `Since ${new Date(sb.cost.since).toLocaleDateString()}` }, num(sb.cost.credits, 2)) : h('span.faint', '—') },
          { label: 'Created', cls: 'hide-md nowrap', render: (sb) => h('span.muted', time(sb.createdAt)) },
          { label: h('span.sr-only', 'Actions'), cls: 'act', render: (sb) => menuButton(() => sandboxMenu(ctx, sb, load), { title: `Actions for ${sb.name}` }) },
        ],
      }));
    });
  }

  async function load() {
    try {
      const r = await api.get('/sandboxes', { limit: 200, include: 'cost' }, { signal: ctx.signal });
      data = r.sandboxes || []; err = null;
      if (data.some((sb) => sb.nodeId && !sb.node)) names = await nodeNames();
      ctx.setNavCount('/sandboxes', data.filter((sb) => isLive(sb.state)).length);
    } catch (e) { if (e.name === 'AbortError') return; err = e; }
    draw();
  }

  // First run: ask once whether other people use these sandboxes (account useCase; "platform"
  // makes the default environment safe for third parties).
  const ask = h('div');
  banner.before(ask);
  api.get('/account-settings').then((st) => {
    if (st.onboarded !== false) return;
    const answer = (useCase) => api.patch('/account-settings', { useCase }).then(() => {
      ask.replaceChildren(); toast(useCase === 'platform' ? 'New sandboxes get none of your secrets, repos or keys' : 'New sandboxes get your environment’s secrets, repos and keys');
    }, toastErr);
    ask.replaceChildren(h('div.notice', { style: { marginBottom: '12px', alignItems: 'center', flexWrap: 'wrap' } }, icon('shield'),
      h('div', { style: { flex: '1 1 260px' } }, h('b', 'Who will use your sandboxes? '), 'If other people (customers, students, the public) drive them, new sandboxes get none of your variables, repositories or AI keys. You can change this per environment later.'),
      h('div.row', btn('Just me or my team', { size: 'sm', kind: 'primary', onClick: () => answer('own') }), btn('Other people too', { size: 'sm', onClick: () => answer('platform') }))));
  }).catch(() => {});

  api.get('/limits').then((l) => {
    if (l && l.canStart === false) banner.replaceChildren(h('div.notice.warn', { style: { marginBottom: '12px' } }, icon('alert'),
      h('div', h('b', 'You cannot start sandboxes right now. '), l.startBlockedReason || l.blockedReason || 'Your credit balance or limits block new starts.', ' ', h('a', { href: '/credits' }, 'See credits'))));
  }).catch(() => {});

  await load();
  every(4000, load, ctx.signal);
  if (ctx.query.get('new') === '1') createSandboxDialog(ctx);
}
