// Nodes: the crowd of contributor machines, their benchmarks and how full they are.
import { h, icon, btn, ago, every, patch, mib, num, plural } from '../dom.js';
import { api, isMissing, errText } from '../api.js';
import { normalizeNode } from '../sbx.js';
import { pageHead, stateChip, tierBadge, catChip, meter, empty, loading, errorBox, segmented, modal, copyable, menuButton, confirmDialog, toast, toastErr, field } from '../ui.js';

const CATS = ['cpu', 'memory', 'disk', 'network', 'gpu'];
const gib = (g) => (g == null ? '—' : `${num(g, g < 10 ? 1 : 0)} GiB`);

export default async function (ctx) {
  ctx.title('Nodes');
  let nodes = null, filter = 'all', sort = 'score', q = '';
  const detail = new Map();
  const stats = h('div'), list = h('div', loading());
  const search = h('input.input', { type: 'search', placeholder: 'Filter by name, owner, CPU, GPU…', 'aria-label': 'Filter nodes', 'data-search': '' });
  search.addEventListener('input', () => { q = search.value.trim().toLowerCase(); draw(); });
  const sortSel = h('select.select', { 'aria-label': 'Sort nodes', style: { width: 'auto' } },
    [['score', 'Sort: benchmark score'], ['free', 'Sort: free capacity'], ['name', 'Sort: name']].map(([v, l]) => h('option', { value: v }, l)));
  sortSel.addEventListener('change', () => { sort = sortSel.value; draw(); });

  ctx.root.append(
    pageHead({
      title: 'Nodes',
      lede: 'The machines your crowd lends to the pool. Each node benchmarks itself and gets a tier; the scheduler places sandboxes where they fit.',
      actions: btn('Add a machine', { kind: 'primary', ico: 'plus', onClick: addMachine, attrs: { 'data-create': '' } }),
    }),
    stats,
    h('div.row.wrap', { style: { margin: '20px 0 12px', gap: '10px' } },
      h('div.search.grow', icon('search'), search),
      segmented([['all', 'All'], ['online', 'Online'], ['offline', 'Offline']], filter, (v) => { filter = v; draw(); }, 'Online filter'),
      sortSel),
    list);

  function draw() {
    if (!nodes) return;
    const on = nodes.filter((n) => n.online);
    const cap = (k) => on.reduce((s, n) => s + (n.info?.capacity?.[k] || 0), 0);
    const use = (k) => on.reduce((s, n) => s + (n.usage?.[k] || 0), 0);
    const gpus = on.reduce((s, n) => s + (n.info?.benchmark?.gpus?.length || 0), 0);
    patch(stats, nodes, () => h('div.grid.cols-4',
      h('div.card.stat', h('span.k', icon('server', 'sm'), 'Nodes online'), h('span.v', on.length, h('small', ` / ${nodes.length}`)), h('span.d', gpus ? plural(gpus, 'GPU') + ' in the pool' : 'No GPUs in the pool')),
      h('div.card.stat', h('span.k', icon('cpu', 'sm'), 'vCPU in use'), h('span.v', use('vcpu'), h('small', ` / ${cap('vcpu')}`)), h('div.bar', h('span', { style: { width: `${pct(use('vcpu'), cap('vcpu'))}%` } }))),
      h('div.card.stat', h('span.k', icon('memory', 'sm'), 'Memory in use'), h('span.v', mib(use('memoryMiB')), h('small', ` / ${mib(cap('memoryMiB'))}`)), h('div.bar', h('span', { style: { width: `${pct(use('memoryMiB'), cap('memoryMiB'))}%` } }))),
      h('div.card.stat', h('span.k', icon('box', 'sm'), 'Sandboxes running'), h('span.v', use('vms'), h('small', ` / ${cap('maxSandboxes') || '∞'}`)), h('span.d', `${gib(use('diskGiB'))} of ${gib(cap('diskGiB'))} disk`))));

    let rows = nodes.filter((n) => (filter === 'all' || (filter === 'online') === !!n.online));
    if (q) rows = rows.filter((n) => [n.name, n.id, n.owner?.login, n.owner?.name, n.owner, n.info?.benchmark?.cpu?.model, n.info?.benchmark?.tier, n.info?.os,
      ...(n.info?.benchmark?.gpus || []).map((g) => g.model), ...Object.entries(n.info?.labels || {}).map(([k, v]) => `${k}=${v}`)]
      .some((s) => s && typeof s === 'string' && s.toLowerCase().includes(q)));
    const free = (n) => (n.info?.capacity?.vcpu || 0) - (n.usage?.vcpu || 0);
    rows.sort((a, b) => (b.online - a.online) || (sort === 'name' ? (a.name || '').localeCompare(b.name || '')
      : sort === 'free' ? free(b) - free(a) : (b.info?.benchmark?.score || 0) - (a.info?.benchmark?.score || 0)));
    ctx.setNavCount('/nodes', on.length);
    patch(list, { rows, n: nodes.length }, () => {
      if (!nodes.length) return h('div.card', empty({ ico: 'server', title: 'No machines in the crowd yet', text: 'Install crowdvm-node on any Linux or Windows machine on your LAN or tailnet and join it to this server.', action: btn('Add a machine', { kind: 'primary', ico: 'plus', onClick: addMachine }) }));
      if (!rows.length) return h('div.card', empty({ ico: 'search', title: 'No matches', text: 'No node matches this filter.' }));
      return h('div.nodes', rows.map((n) => nodeCard(n, canManage(n) ? { menu: manageMenu(n), cancelEvac: () => cancelEvacuation(n) } : null)));
    });
  }

  async function load() {
    try {
      const r = await api.get('/nodes', undefined, { signal: ctx.signal });
      // Servers that list a summary per node: fetch each node's full benchmark (cached a minute).
      nodes = await Promise.all((r.nodes || []).map(async (n) => {
        if (n.info?.benchmark || n.benchmark) return normalizeNode(n);
        const c = detail.get(n.id);
        if (c && Date.now() - c.at < 60000) return normalizeNode({ ...n, benchmark: c.b });
        const d = await api.get(`/nodes/${encodeURIComponent(n.id)}`).catch(() => null);
        const b = d?.node?.benchmark || d?.benchmark;
        if (b) detail.set(n.id, { at: Date.now(), b });
        return normalizeNode(b ? { ...n, benchmark: b } : n);
      }));
      draw();
    }
    catch (e) { if (e.name !== 'AbortError' && !nodes) list.replaceChildren(errorBox(e, load)); }
  }

  const me = ctx.session.me || {};
  const canManage = (n) => me.isAdmin || (n.ownerId || n.owner?.id) === me.id;
  const update = (n, body, msg) => api.patch(`/nodes/${encodeURIComponent(n.id)}`, body).then(() => { toast(msg); load(); }, toastErr);
  function manageMenu(n) {
    return menuButton(() => [
      evacuating(n) ? null // drained until the evacuation ends
        : n.disabled ? { label: 'Resume scheduling', ico: 'play', onClick: () => update(n, { disabled: false }, `${n.name} takes new sandboxes again`) }
          : { label: 'Drain (no new sandboxes)', ico: 'pause', onClick: () => update(n, { disabled: true }, `${n.name} takes no new sandboxes`) },
      !evacuating(n) ? { label: 'Evacuate…', ico: 'upload', onClick: () => evacuate(n) }
        : inGrace(n) ? { label: 'Cancel evacuation', ico: 'x', onClick: () => cancelEvacuation(n) } : null,
      { label: 'Rename…', ico: 'edit', onClick: () => editNode(n, 'name') },
      { label: 'Labels…', ico: 'edit', onClick: () => editNode(n, 'labels') },
      '-',
      { label: 'Remove from the crowd…', ico: 'trash', danger: true, onClick: () => removeNode(n) },
    ], { title: `Manage ${n.name || n.id}` });
  }
  function editNode(n, what) {
    const labels = n.labels || n.info?.labels || {};
    const input = what === 'name' ? h('input.input', { value: n.name || '' })
      : h('textarea.textarea.mono', { rows: 4, value: Object.entries(labels).map(([k, v]) => `${k}=${v}`).join('\n'), placeholder: 'room=lab\ngpu=rtx' });
    modal({ title: what === 'name' ? `Rename ${n.name}` : `Labels of ${n.name}`, submit: what === 'name',
      body: field(what === 'name' ? 'Name' : 'Labels (key=value, one per line)', input, what === 'name' ? null : 'Sandboxes can ask for nodes with a label (placement).'),
      actions: [{ label: 'Cancel', onClick: () => true }, { label: 'Save', kind: 'primary', submit: what === 'name', onClick: async () => {
        if (what === 'name') { if (!input.value.trim()) throw new Error('Enter a name.'); await api.patch(`/nodes/${encodeURIComponent(n.id)}`, { name: input.value.trim() }); }
        else {
          const m = {};
          for (const l of input.value.split('\n').map((x) => x.trim()).filter(Boolean)) { const i = l.indexOf('='); if (i < 1) throw new Error(`Not key=value: ${l}`); m[l.slice(0, i).trim()] = l.slice(i + 1).trim(); }
          await api.patch(`/nodes/${encodeURIComponent(n.id)}`, { labels: m });
        }
        toast('Node saved'); load();
      } }] });
  }
  // Evacuate: drain, then after the grace period stop the sandboxes (disks
  // saved) and resume them on other nodes.
  async function evacuate(n) {
    let grace = 600;
    const warn = h('div');
    modal({
      title: `Evacuate ${n.name}?`,
      desc: 'Takes the machine back. It stops taking new sandboxes at once. When the grace period ends, every sandbox still on it is stopped, which saves its disk, and resumed on another node. Running processes are lost. Their owners get a notice now.',
      body: h('div.stack',
        field('Stop the sandboxes', segmented([[0, 'Now'], [600, 'In 10 min'], [3600, 'In 1 hour']], grace, (v) => { grace = v; }, 'Grace period')),
        warn),
      actions: [{ label: 'Cancel', onClick: () => true }, { label: 'Evacuate', kind: 'danger solid', onClick: async () => {
        await api.post(`/nodes/${encodeURIComponent(n.id)}/evacuate`, { graceSec: grace });
        toast(grace ? `${n.name} is evacuating` : `Moving the sandboxes off ${n.name}`); load();
      } }],
    });
    const d = await api.get(`/nodes/${encodeURIComponent(n.id)}`).catch(() => null);
    const k = d?.node?.noSnapshotSandboxes || 0;
    if (k > 0) warn.replaceChildren(h('div.notice.warn', icon('alert'), h('div', `${plural(k, 'sandbox', 'sandboxes')} ${k === 1 ? 'has' : 'have'} snapshots off and will be erased: ${k === 1 ? 'it' : 'they'} can't be moved.`)));
  }
  function cancelEvacuation(n) {
    api.del(`/nodes/${encodeURIComponent(n.id)}/evacuate`).then(() => { toast(`Evacuation of ${n.name} cancelled. It stays drained.`); load(); }, toastErr);
  }
  async function removeNode(n) {
    const busy = (n.usage?.vms || n.sandboxes || 0) > 0;
    if (busy && !evacuating(n)) {
      let next = null;
      await new Promise((resolve) => modal({
        title: `Remove ${n.name}?`,
        desc: `It still runs ${plural(n.usage?.vms || n.sandboxes, 'sandbox', 'sandboxes')}. Evacuate first to stop them with their disks saved and resume them on other nodes. Removing now cuts them off; they are rebuilt elsewhere from their last snapshot and lose recent changes.`,
        actions: [{ label: 'Cancel', onClick: () => true },
          { label: 'Remove anyway…', kind: 'danger', onClick: () => { next = 'remove'; } },
          { label: 'Evacuate first…', kind: 'primary', submit: true, onClick: () => { next = 'evacuate'; } }],
        onClose: resolve,
      }));
      if (next === 'evacuate') return evacuate(n);
      if (next !== 'remove') return;
    }
    if (!(await confirmDialog({ title: `Remove ${n.name}?`, danger: true, confirm: 'Remove', typeToConfirm: busy ? n.name : undefined,
      desc: busy ? 'It still runs sandboxes. They lose their machine and are rebuilt on other nodes from their last snapshot; changes since then are lost.' : 'It disconnects and must join again with a new token to come back.' }))) return;
    const del = (force) => api.del(`/nodes/${encodeURIComponent(n.id)}`, force ? { force: 'true' } : undefined);
    const done = () => { toast(`Removed ${n.name}`); detail.delete(n.id); load(); };
    try { await del(busy); done(); } catch (e) {
      // Snapshots only this node holds (not uploaded yet): wait, or lose them.
      if (e.code !== 'node_holds_snapshots') return toastErr(e);
      const k = e.details?.snapshots || 0;
      if (!(await confirmDialog({ title: `Remove ${n.name} anyway?`, danger: true, confirm: 'Remove anyway',
        desc: `${k ? plural(k, 'snapshot', 'snapshots') : 'Some snapshots'} exist only on this node and are still uploading. Wait a few minutes and try again, or remove it now and lose them: their sandboxes go back to an older snapshot or can't be restored.` }))) return;
      try { await del(true); done(); } catch (e2) { toastErr(e2); }
    }
  }

  async function addMachine() {
    const tokenBox = h('div', loading('Creating a join token…'));
    modal({
      title: 'Add a machine to the crowd', wide: true,
      desc: 'Lend a Linux or Windows machine on this LAN or tailnet. It benchmarks itself, then hosts sandboxes within the limits its owner sets.',
      body: h('div.stack',
        h('ol.stack.tight', { style: { margin: 0, paddingLeft: '18px' } },
          h('li', 'Install ', h('code', 'crowdvm-node'), ' and QEMU on the machine (KVM on Linux, WHPX on Windows).'),
          h('li', 'Join it to this server:'),
          tokenBox,
          h('li', 'Optionally cap what you lend: ', h('code', 'crowdvm-node limits --vcpu 8 --memory 16G --disk 200G')),
          h('li', 'It shows up here within a few seconds, with its benchmark and tier.'))),
      actions: [{ label: 'Done', kind: 'primary', onClick: () => true }],
    });
    try {
      const r = await api.post('/join-tokens', { name: 'dashboard' });
      tokenBox.replaceChildren(copyable(r.command || `crowdvm-node join --server ${location.origin} --token ${r.token}`, { secret: true }),
        h('p.small.faint', { style: { marginTop: '6px' } }, r.expiresAt ? `Token works until ${new Date(r.expiresAt).toLocaleString()}.` : 'Keep the token private.'));
    } catch (e) {
      tokenBox.replaceChildren(copyable(`crowdvm-node join --server ${location.origin} --token <join token>`),
        h('p.small.faint', { style: { marginTop: '6px' } }, isMissing(e) || e.status === 403 ? 'Only server admins create join tokens here. Ask one for a token.' : errText(e)));
    }
  }

  await load();
  every(10000, load, ctx.signal);
}

const pct = (u, t) => (t > 0 ? Math.min(100, (u / t) * 100) : 0);
const evacuating = (n) => !!n.evacuation;
const inGrace = (n) => !!n.evacuation && new Date(n.evacuation.deadline) > Date.now();
const hhmm = (t) => new Date(t).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
const PAUSED = { owner: 'paused by its owner', schedule: 'outside its schedule', hold: 'on hold' };

// stateChips: drained, evacuating (with Cancel during the grace period) and
// paused by the node's owner.
function stateChips(n, manage) {
  const ev = n.evacuation, grace = inGrace(n);
  return [
    n.disabled && !ev ? h('span.chip.warn.plain', { title: 'Drained: takes no new sandboxes' }, 'drained') : null,
    ev ? h('span.chip.warn', { title: grace ? 'Its sandboxes are stopped and moved to other nodes at this time' : 'Moving its sandboxes and their snapshots off this node' },
      grace ? `evacuating · until ${hhmm(ev.deadline)}` : 'evacuating') : null,
    ev && grace && manage ? btn('Cancel', { size: 'sm', kind: 'ghost', title: 'Cancel the evacuation (the node stays drained)', onClick: manage.cancelEvac }) : null,
    n.paused || n.info?.paused ? h('span.chip.plain', { title: 'Takes no new sandboxes until its owner resumes it; running ones continue' }, PAUSED[n.pauseReason || n.info?.pauseReason] || 'paused') : null,
  ];
}

function nodeCard(n, manage) {
  const menu = manage?.menu;
  const info = n.info || {}, b = info.benchmark || {}, cap = info.capacity || {}, use = n.usage || {};
  const cats = b.categories || {};
  const owner = typeof n.owner === 'string' ? n.owner : n.owner?.displayName || n.owner?.login || n.owner?.name || n.ownerLogin;
  const gpu = (b.gpus || [])[0];
  const virt = b.virtualization || {};
  const metric = (k, v, sub) => h('div', h('div.k', k), h('div.v', v), sub ? h('div.k', sub) : null);
  return h('article.card.node', { class: n.online ? '' : 'offline', 'aria-label': `${n.name}, ${n.online ? 'online' : 'offline'}` },
    h('div.card-head',
      h('div', { style: { minWidth: 0 } },
        h('div.name', h('span.truncate', n.name || n.id), stateChip(n.online ? 'online' : 'offline'), stateChips(n, manage)),
        h('div.hw', [owner ? `${owner}’s` : null, `${info.os || '?'}/${info.arch || '?'}`, info.driver === 'process' ? 'process driver (no isolation)' : virt.accel ? virt.accel.toUpperCase() : null, info.version ? `v${info.version}` : null].filter(Boolean).join(' · '))),
      h('div.row', { style: { alignItems: 'flex-start', gap: '10px' } },
        h('div.score', { title: 'Overall benchmark score (0–1000)' }, h('b', b.score != null ? num(b.score) : '—'), h('span', 'score')),
        tierBadge(b.tier), menu)),
    h('div.card-body.stack',
      h('div.cats', CATS.map((c) => catChip(c, cats[c] || (c === 'gpu' && !(b.gpus || []).length ? 'none' : cats[c])))),
      h('div.bench',
        metric('CPU', b.cpu ? `${b.cpu.cores}c / ${b.cpu.threads}t` : '—', h('span.truncate', { title: b.cpu?.model, style: { display: 'block' } }, b.cpu?.model || '')),
        metric('CPU score', b.cpu ? `${num(b.cpu.singleScore / 1000, 1)}k` : '—', b.cpu ? `${num(b.cpu.multiScore / 1000, 0)}k multi` : ''),
        metric('Memory', b.memory ? mib(b.memory.totalMiB) : '—', b.memory?.bandwidthMBps ? `${num(b.memory.bandwidthMBps / 1000, 1)} GB/s` : ''),
        metric('Disk', b.disk ? `${num(b.disk.readMBps)} / ${num(b.disk.writeMBps)}` : '—', 'MB/s read / write'),
        metric('Network', b.network ? `${num(b.network.downMbps)}↓ ${num(b.network.upMbps)}↑` : '—', b.network ? `Mbps · ${num(b.network.latencyMs, 1)} ms · ${b.network.via || ''}` : ''),
        metric('GPU', gpu ? h('span.truncate', { title: gpu.model, style: { display: 'block' } }, gpu.model) : h('span.faint', 'none'), gpu ? `${mib(gpu.vramMiB)}${(b.gpus || []).length > 1 ? ` · +${b.gpus.length - 1}` : ''}` : virt.nested ? 'nested virt' : '')),
      h('div.bars',
        meter('vCPU', use.vcpu, cap.vcpu),
        meter('Memory', use.memoryMiB, cap.memoryMiB, mib),
        meter('Disk', use.diskGiB, cap.diskGiB, gib),
        meter('Sandboxes', use.vms, cap.maxSandboxes)),
      manage ? h('p.small.faint', { style: { margin: 0 } }, 'Control this machine locally: run ', h('code', 'crowdvm-node open'), ' on it.') : null),
    h('div.card-foot', { style: { justifyContent: 'space-between', background: 'transparent' } },
      h('span.small.faint.truncate', n.online ? (b.ranAt ? `Benchmarked ${ago(b.ranAt)}` : 'Not benchmarked yet') : `Last seen ${ago(n.lastSeenAt)}`),
      h('span.row', { style: { gap: '4px' } }, Object.entries(info.labels || {}).slice(0, 3).map(([k, v]) => h('span.tag', `${k}=${v}`)),
        info.directHost ? h('span.tag', { title: 'Publishes direct LAN ports' }, info.directHost) : null)));
}
