// Sandbox actions and dialogs shared by the list, detail and snapshot pages.
import { h, icon, btn, duration, parseTTL, mib, num, copy } from './dom.js';
import { api, isMissing, errText } from './api.js';
import { modal, toast, toastErr, confirmDialog, field, segmented, isLive, isStopped, isBusy } from './ui.js';

// The server's GET /nodes may return NodeInfo nested under `info` or flattened; accept both.
export function normalizeNode(n) {
  const i = n.info || {};
  const b = { ...(n.benchmark || i.benchmark || {}) };
  b.tier ??= n.tier; b.score ??= n.score; b.categories ??= n.categories;
  if (!b.virtualization) b.virtualization = { accel: n.accel, nested: n.nested };
  if (!b.gpus && typeof n.gpus === 'number') b.gpus = Array.from({ length: n.gpus }, () => ({ model: 'GPU' }));
  return {
    ...n,
    owner: n.owner || n.ownerLogin || null,
    info: { os: n.os, arch: n.arch, version: n.version, driver: n.driver, labels: n.labels, capacity: n.capacity, directHost: n.directHost, ...i, benchmark: b },
    usage: n.usage || {},
  };
}

// id -> node name, cached for a minute (sandboxes carry only nodeId on some servers).
let nodeCache = null, nodeCacheAt = 0;
export async function nodeNames() {
  if (!nodeCache || Date.now() - nodeCacheAt > 60000) {
    nodeCacheAt = Date.now();
    nodeCache = api.get('/nodes').then((r) => Object.fromEntries((r.nodes || []).map((n) => [n.id, normalizeNode(n)]))).catch(() => ({}));
  }
  return nodeCache;
}
export function nodeOf(sb, names = {}) {
  if (sb.node) return sb.node;
  if (!sb.nodeId) return null;
  const n = names[sb.nodeId];
  return { id: sb.nodeId, name: n?.name || sb.nodeId, tier: n?.info?.benchmark?.tier };
}

export const SIZES = [
  { name: 'small', vcpu: 2, memoryMiB: 4096, diskGiB: 51, multiplier: 0.5 },
  { name: 'default', vcpu: 4, memoryMiB: 8192, diskGiB: 51, multiplier: 1 },
  { name: 'large', vcpu: 8, memoryMiB: 16384, diskGiB: 111, multiplier: 2 },
  { name: 'xlarge', vcpu: 16, memoryMiB: 32768, diskGiB: 251, multiplier: 5.5 },
];
const TTLS = [['1800', '30m'], ['3600', '1h'], ['14400', '4h'], ['86400', '1d'], ['never', 'Never'], ['custom', 'Custom']];
export const sizeText = (sb) => {
  const s = SIZES.find((x) => x.name === sb.type);
  return s ? `${s.vcpu} vCPU · ${mib(s.memoryMiB)}` : `${sb.vcpu ?? '?'} vCPU · ${sb.memoryGB ?? '?'} GB`;
};

function sizePicker(value = 'default') {
  return h('div.size-picker', { role: 'radiogroup', 'aria-label': 'Machine size' }, SIZES.map((s) =>
    h('label.size-opt', h('input', { type: 'radio', name: 'size', value: s.name, checked: s.name === value }),
      h('span.n', s.name), h('span.s', `${s.vcpu} vCPU · ${mib(s.memoryMiB)}`), h('span.m', `${s.diskGiB} GB disk · ${s.multiplier}× credits`))));
}

function ttlControl(initial = '3600') {
  const custom = h('input.input.sm', { placeholder: 'e.g. 90m, 6h, 3d', hidden: initial !== 'custom', style: { maxWidth: '160px' }, 'aria-label': 'Custom auto-stop' });
  const seg = segmented(TTLS, initial, (v) => { custom.hidden = v !== 'custom'; if (v === 'custom') custom.focus(); }, 'Auto-stop after');
  const el = h('div.row.wrap', seg, custom);
  el.read = () => {
    const v = seg.value;
    if (v === 'never') return null;
    if (v !== 'custom') return Number(v);
    const s = parseTTL(custom.value);
    if (s === undefined) { custom.setAttribute('aria-invalid', 'true'); throw new Error('Auto-stop: use a number with s, m, h or d, like 90m.'); }
    return s;
  };
  return el;
}

function parseKV(text, what) {
  const out = {};
  for (const raw of text.split('\n')) {
    const line = raw.trim();
    if (!line || line.startsWith('#')) continue;
    const i = line.indexOf('=');
    if (i < 1) throw new Error(`${what}: “${line}” needs KEY=value.`);
    out[line.slice(0, i).trim()] = line.slice(i + 1).trim();
  }
  return out;
}

// ---------------- create ----------------
export async function createSandboxDialog(ctx, preset = {}) {
  const active = ctx.session.view; // the viewed organization; the create bills it (X-CrowdVM-Org)
  const [envs, snaps, nodes, orgEnvs] = await Promise.allSettled([api.get('/environments'), api.get('/saved-snapshots'), api.get('/nodes'),
    active?.type === 'org' ? api.get('/environments', { org: active.id }) : Promise.resolve(null)]);
  // Names resolve to your own environment first, then the viewed organization's.
  const own = envs.value?.environments || [];
  const envList = [...own, ...(orgEnvs.value?.environments || []).filter((e) => !own.some((x) => x.name === e.name)).map((e) => ({ ...e, isDefault: false, orgName: active.name }))];
  const snapList = (snaps.value?.snapshots || []).filter((s) => s.status === 'ready');
  const nodeList = nodes.value?.nodes || [];
  const org = active;

  const name = h('input.input', { name: 'name', placeholder: 'Optional, e.g. checkout-refactor', maxlength: 120, autocomplete: 'off' });
  const sizes = sizePicker(preset.type || 'default');
  const ttl = ttlControl();

  const snapSel = h('select.select', snapList.length ? snapList.map((s) => h('option', { value: s.name, selected: s.name === preset.from }, `${s.name}${s.type ? ` · ${s.type}` : ''}`)) : h('option', { value: '' }, 'No saved snapshots yet'));
  const codeIn = h('input.input.mono', { placeholder: 'bsh_…', autocomplete: 'off', spellcheck: 'false' });
  const srcBox = h('div');
  let src = preset.from ? 'snapshot' : preset.code ? 'code' : 'base';
  const drawSrc = () => srcBox.replaceChildren(
    src === 'snapshot' ? field('Saved snapshot', snapSel, 'The sandbox starts from that exact frozen disk.')
      : src === 'code' ? field('Share code', codeIn, 'A code someone made with “Share” on a saved snapshot.') : h('p.small.faint', 'Fresh Ubuntu 24.04 with Docker, a desktop, browsers, language toolchains and coding agents.'));
  const srcSeg = segmented([['base', 'Base image'], ['snapshot', 'Saved snapshot'], ['code', 'Share code']], src, (v) => { src = v; drawSrc(); }, 'Start from');
  drawSrc();

  const envSel = h('select.select', envList.map((e) => h('option', { value: e.name, selected: e.isDefault }, `${e.name}${e.isDefault ? ' (default)' : ''}${e.orgName ? ` · ${e.orgName}` : ''}`)),
    h('option', { value: '__none' }, 'No environment (pass nothing)'));

  const nodeSel = h('select.select', h('option', { value: '' }, 'Any node (scheduler picks)'),
    nodeList.map(normalizeNode).map((n) => h('option', { value: n.id, disabled: !n.online }, `${n.name || n.id}${n.online ? '' : ' (offline)'}${n.info.benchmark.tier ? ` · ${n.info.benchmark.tier}` : ''}`)));
  const tierSel = h('select.select', [['', 'Any tier'], ['silver', 'Silver or better'], ['gold', 'Gold or better'], ['platinum', 'Platinum only']].map(([v, l]) => h('option', { value: v }, l)));
  const gpu = h('input', { type: 'checkbox' });
  const nested = h('input', { type: 'checkbox' });
  const labels = h('textarea.textarea.mono', { rows: 2, placeholder: 'task=checkout\nowner=ci' });
  const envVars = h('textarea.textarea.mono', { rows: 2, placeholder: 'FEATURE_FLAG=1' });
  const setup = h('textarea.textarea.mono', { rows: 3, placeholder: '# runs once, as user, after the sandbox is ready\nnpm ci' });
  const vis = org && org.type === 'org' ? segmented([['', 'Org default'], ['shared', 'Shared'], ['private', 'Private']], '', null, 'Visibility') : null;

  const body = h('div.stack.loose',
    h('div.fields',
      field('Name', name, null, { full: true }),
      h('div.field.full', h('span.label', 'Size'), sizes),
      h('div.field.full', h('span.label', 'Start from'), srcSeg, srcBox),
      field('Environment', envSel, envList.length ? 'Variables, secret files and repos the sandbox gets.' : 'Uses your default environment.'),
      h('div.field', h('span.label', 'Auto-stop after'), ttl, h('div.hint', 'Stopped sandboxes keep their disk and resume on any node.'))),
    h('details.more', h('summary', icon('chevron', 'sm'), 'Placement, labels, env and setup'),
      h('div.stack',
        h('div.fields',
          field('Node', nodeSel, 'Pin to one contributor machine.'),
          field('Minimum node tier', tierSel, 'From each node’s benchmark.'),
          h('label.check', gpu, h('div', h('div.t', 'Needs a GPU'), h('div.d', 'Only nodes that report a GPU.'))),
          h('label.check', nested, h('div', h('div.t', 'Needs nested virtualization'), h('div.d', 'For Docker-in-VM tools like Firecracker or KVM.')))),
        h('div.fields',
          field('Labels', labels, 'One key=value per line.'),
          field('Environment variables', envVars, 'One KEY=value per line. Wins over the environment.')),
        field('Setup script', setup, 'Bash. Runs once in the background; see setup status on the sandbox.'),
        vis ? h('div.field', h('span.label', 'Visibility in the organization'), vis) : null)),
  );

  return new Promise((resolve) => {
    modal({
      title: 'New sandbox', desc: org && org.type === 'org' ? `Billed to ${org.name}.` : 'A persistent Ubuntu VM on a node in your crowd.', wide: true, body,
      submit: true,
      actions: [
        { label: 'Cancel', onClick: () => true },
        { label: 'Create sandbox', kind: 'primary', ico: 'plus', submit: true, onClick: async () => {
          const b = { type: sizes.querySelector('input:checked')?.value || 'default', ttlSeconds: ttl.read() };
          if (name.value.trim()) b.name = name.value.trim();
          if (src === 'snapshot') { if (!snapSel.value) throw new Error('Pick a saved snapshot, or start from the base image.'); b.from = snapSel.value; }
          if (src === 'code') { if (!codeIn.value.trim()) throw new Error('Paste a share code.'); b.from = codeIn.value.trim(); }
          if (envSel.value === '__none') b.noEnv = true; else if (envSel.value) b.environment = envSel.value;
          const placement = {};
          if (nodeSel.value) placement.node = nodeSel.value;
          if (tierSel.value) placement.minTier = tierSel.value;
          if (gpu.checked) placement.gpu = true;
          if (nested.checked) placement.nested = true;
          if (Object.keys(placement).length) b.placement = placement;
          const l = parseKV(labels.value, 'Labels'); if (Object.keys(l).length) b.labels = l;
          const e = parseKV(envVars.value, 'Environment variables'); if (Object.keys(e).length) b.env = e;
          if (setup.value.trim()) b.setupScript = setup.value;
          if (vis?.value) b.visibility = vis.value;
          const r = await api.post('/sandboxes', b, { headers: { 'Idempotency-Key': crypto.randomUUID?.() || String(Date.now()) } });
          toast(`Creating ${r.sandbox?.name || r.sandbox?.id}`);
          resolve(r.sandbox);
          ctx.nav(`/sandboxes/${r.sandbox.id}`);
        } },
      ],
      onClose: () => resolve(null),
    });
    name.focus();
  });
}

// ---------------- lifecycle actions ----------------
export async function stopSandbox(sb, force = false) {
  const ok = await confirmDialog({
    title: force ? `Force stop ${sb.name}?` : `Stop ${sb.name}?`,
    desc: force ? 'Powers off now. Changes since the last snapshot are lost.' : 'The disk is saved as a snapshot. Running processes end. Resume it later on any node.',
    confirm: force ? 'Force stop' : 'Stop sandbox', danger: force,
  });
  if (!ok) return false;
  try { await api.post(`/sandboxes/${sb.id}/stop`, force ? { force: true } : {}); toast(`Stopping ${sb.name}`); return true; }
  catch (e) { toastErr(e); return false; }
}

export async function resumeSandbox(sb) {
  try { await api.post(`/sandboxes/${sb.id}/resume`, {}); toast(`Resuming ${sb.name}`); return true; }
  catch (e) { toastErr(e); return false; }
}

// The sandbox's current auto-stop as a ttlControl value (null = never).
const ttlOf = (sb) => (sb.ttlSeconds == null ? 'never' : TTLS.find(([v]) => Number(v) === sb.ttlSeconds)?.[0] || '3600');

export function resumeDialog(sb) {
  const sizes = sizePicker(sb.type || 'default');
  const ttl = ttlControl(ttlOf(sb));
  return new Promise((resolve) => modal({
    title: `Resume ${sb.name}`, desc: 'Boots from the latest snapshot, on any node that fits.', wide: true,
    body: h('div.stack', h('div.field', h('span.label', 'Size'), sizes), h('div.field', h('span.label', 'Auto-stop after'), ttl)),
    actions: [{ label: 'Cancel', onClick: () => true }, { label: 'Resume', kind: 'primary', ico: 'play', submit: true, onClick: async () => {
      await api.post(`/sandboxes/${sb.id}/resume`, { type: sizes.querySelector('input:checked').value, ttlSeconds: ttl.read() });
      toast(`Resuming ${sb.name}`); resolve(true);
    } }],
    onClose: () => resolve(false),
  }));
}

export function forkDialog(ctx, sb) {
  const name = h('input.input', { value: `${sb.name} (fork)`, maxlength: 120 });
  const sizes = sizePicker(sb.type || 'default');
  const ttl = ttlControl();
  modal({
    title: `Fork ${sb.name}`, wide: true, submit: true,
    desc: isLive(sb.state) ? 'Takes a live snapshot and boots a copy next to it. The original keeps running.' : 'Boots a copy from its latest snapshot.',
    body: h('div.stack', field('Name', name), h('div.field', h('span.label', 'Size'), sizes), h('div.field', h('span.label', 'Auto-stop after'), ttl)),
    actions: [{ label: 'Cancel', onClick: () => true }, { label: 'Fork', kind: 'primary', ico: 'fork', submit: true, onClick: async () => {
      const r = await api.post(`/sandboxes/${sb.id}/fork`, { name: name.value.trim() || undefined, type: sizes.querySelector('input:checked').value, ttlSeconds: ttl.read() });
      const nb = r.sandbox || r;
      toast(`Forked into ${nb.name || nb.id}`);
      if (nb.id) ctx.nav(`/sandboxes/${nb.id}`);
    } }],
  });
}

export async function recoverSandbox(sb) {
  const ok = await confirmDialog({ title: `Recover ${sb.name}?`, desc: 'Restarts a hung machine. The disk is kept; running processes end.', confirm: 'Recover' });
  if (!ok) return false;
  try { await api.post(`/sandboxes/${sb.id}/recover`, {}); toast('Recovering'); return true; } catch (e) { toastErr(e); return false; }
}

export async function deleteSandbox(sb) {
  const ok = await confirmDialog({
    title: `Delete ${sb.name}?`, danger: true, confirm: 'Delete permanently', typeToConfirm: sb.name.length <= 40 ? sb.name : sb.id,
    desc: 'Deletes the machine, its snapshots and command history. Saved snapshots made from it stay. This cannot be undone.',
  });
  if (!ok) return false;
  try { await api.del(`/sandboxes/${sb.id}`, undefined, { headers: { 'X-CrowdVM-Confirm-Delete': sb.id } }); toast(`Deleted ${sb.name}`); return true; } catch (e) { toastErr(e); return false; }
}

export function extendDialog(sb) {
  const ttl = ttlControl(ttlOf(sb));
  const left = sb.archiveAfter ? Math.max(0, (new Date(sb.archiveAfter) - Date.now()) / 1000) : null;
  return new Promise((resolve) => modal({
    title: 'Auto-stop', desc: left == null ? 'Auto-stop is off.' : `Stops by itself in ${duration(left)}. The new time counts from now.`,
    body: h('div.field', h('span.label', 'Stop after'), ttl),
    actions: [{ label: 'Cancel', onClick: () => true }, { label: 'Save', kind: 'primary', submit: true, onClick: async () => {
      await api.patch(`/sandboxes/${sb.id}`, { ttlSeconds: ttl.read() }); toast('Auto-stop updated'); resolve(true);
    } }],
    onClose: () => resolve(false),
  }));
}

export function renameDialog(sb) {
  const name = h('input.input', { value: sb.name, maxlength: 120 });
  const sub = h('input.input.mono', { value: sb.subdomain || '', maxlength: 40, autocomplete: 'off', spellcheck: 'false', placeholder: 'my-app' });
  return new Promise((resolve) => {
    modal({
      title: 'Rename sandbox', submit: true,
      body: h('div.stack', field('Name', name),
        sb.subdomain !== undefined ? field('Subdomain', sub, 'Public URLs become <subdomain>-<port>.<public domain>. 3–40 lowercase letters, digits and hyphens.') : null),
      actions: [{ label: 'Cancel', onClick: () => true }, { label: 'Save', kind: 'primary', submit: true, onClick: async () => {
        if (!name.value.trim()) throw new Error('Name cannot be empty.');
        const body = { name: name.value.trim() };
        if (sb.subdomain !== undefined && sub.value.trim() !== (sb.subdomain || '')) body.subdomain = sub.value.trim();
        await api.patch(`/sandboxes/${sb.id}`, body); resolve(true);
      } }],
      onClose: () => resolve(false),
    });
    name.select();
  });
}

export function saveSnapshotDialog(sb) {
  const name = h('input.input.mono', { placeholder: 'my-template', pattern: '[a-z0-9][a-z0-9-]{0,62}', autocomplete: 'off', spellcheck: 'false' });
  modal({
    title: 'Save as snapshot', submit: true,
    desc: 'Freezes the current disk under a name. Create new sandboxes from it, or share it with a code.',
    body: field('Name', name, 'Lowercase letters, digits and dashes. Reusing a name replaces it.'),
    actions: [{ label: 'Cancel', onClick: () => true }, { label: 'Save snapshot', kind: 'primary', ico: 'camera', submit: true, onClick: async () => {
      if (!/^[a-z0-9][a-z0-9-]{0,62}$/.test(name.value)) { name.setAttribute('aria-invalid', 'true'); throw new Error('Use 1–63 lowercase letters, digits or dashes, starting with a letter or digit.'); }
      await api.post('/saved-snapshots', { sandboxId: sb.id, name: name.value });
      toast(`Saving snapshot ${name.value}`);
    } }],
  });
}

export async function toggleShare(sb) {
  const share = sb.visibility !== 'shared';
  const ok = await confirmDialog({
    title: share ? 'Share with the organization?' : 'Make private?',
    desc: share ? 'Every member can then open, run commands in, prompt, resume and stop it. Your personal logins are wiped at its next start.' : 'Only you can use it. Members can still see and stop it.',
    confirm: share ? 'Share' : 'Make private',
  });
  if (!ok) return false;
  try { await api.post(`/sandboxes/${sb.id}/${share ? 'share' : 'unshare'}`, {}); toast(share ? 'Shared with the organization' : 'Now private'); return true; }
  catch (e) { toastErr(e); return false; }
}

// Chrome in the sandbox, full screen in a new tab (POST /browser).
export async function openBrowser(sb) {
  const w = window.open('about:blank', '_blank');
  try {
    const r = await api.post(`/sandboxes/${sb.id}/browser`, {});
    const u = r.viewerUrl || r.browserUrl;
    if (w) { w.opener = null; w.location.href = u; } else window.open(u, '_blank', 'noopener');
  } catch (e) { w?.close(); toastErr(e); }
}

// Proxied AI requests of one sandbox; block or allow its AI access.
export function aiUsageDialog(sb) {
  const box = h('div', h('div.loading', h('span.spinner'), 'Loading…'));
  let blocked = false, last = {};
  const toggle = btn('', { size: 'sm' });
  const draw = (r) => {
    r = last = { ...last, ...r };
    blocked = !!r.blocked;
    const t = r.totals || {};
    toggle.textContent = blocked ? 'Allow AI access' : 'Block AI access';
    toggle.className = `btn sm ${blocked ? 'primary' : 'danger'}`;
    box.replaceChildren(h('div.stack',
      h('div.row.between', h('span.small.muted', `Since ${r.since || '—'}${r.proxied ? '' : '. This sandbox does not use the credentials proxy, so its requests are not counted.'}`), toggle),
      blocked ? h('div.notice.warn', icon('lock'), h('div', 'AI access is blocked: proxied requests from this sandbox are refused.')) : null,
      h('div.grid.cols-3', { style: { gap: '8px' } },
        h('div.card.stat', h('span.k', 'Requests'), h('span.v', num(t.requests)), t.errors ? h('span.d', `${num(t.errors)} errors`) : null),
        h('div.card.stat', h('span.k', 'Tokens in / out'), h('span.v', { style: { fontSize: '15px' } }, `${num(t.inputTokens)} / ${num(t.outputTokens)}`)),
        h('div.card.stat', h('span.k', 'Cost'), h('span.v', `$${num(r.dollars, 2)}`))),
      (r.usage || []).length ? h('div.table-wrap', h('table.t', h('thead', h('tr', ['Provider', 'Model', 'Requests', 'Tokens'].map((x) => h('th', { scope: 'col' }, x)))),
        h('tbody', r.usage.map((u) => h('tr', h('td', u.provider || u.credential || '—'), h('td', h('code.small', u.model || '—')), h('td.num', num(u.requests)), h('td.num', num((u.inputTokens || 0) + (u.outputTokens || 0)))))))) : null));
  };
  toggle.addEventListener('click', async () => {
    try { const x = await api.put(`/sandboxes/${sb.id}/ai-access`, { blocked: !blocked }); draw({ blocked: x.blocked }); toast(blocked ? 'AI access blocked' : 'AI access allowed'); } catch (e) { toastErr(e); }
  });
  modal({ title: `AI usage of ${sb.name}`, wide: true, body: box, actions: [{ label: 'Close', onClick: () => true }] });
  api.get(`/sandboxes/${sb.id}/ai-usage`).then(draw, (e) => box.replaceChildren(h('div.notice.err', icon('alert'), h('div', errText(e)))));
}

// What the caller may do with a teammate's sandbox (the API's `access`): owner = everything,
// use = everything but the creator-only actions, view = look and stop only.
export const sbAccess = (sb) => sb.access || 'owner';
export const CREATOR_ONLY = 'Only its creator can do this.';

// Boat's sharing labels for an organization sandbox: view only (a teammate's private one),
// private or shared (your own). Personal sandboxes have no visibility and get none.
export function shareLabels(sb) {
  const acc = sbAccess(sb);
  if (acc === 'view') return h('span.chip.plain.warn', { title: 'Private to its creator. You can look at it and stop it.' }, 'view only');
  if (acc !== 'owner' || !sb.visibility) return null;
  return sb.visibility === 'private'
    ? h('span.chip.plain', { title: 'Your teammates see it as view only. Pick Share with org in the row menu to share it.' }, 'private')
    : h('span.chip.plain', { title: 'Every member of the organization can use it.' }, 'shared');
}

// Menu items for one sandbox (list rows and the detail header).
export function sandboxMenu(ctx, sb, after) {
  const done = (p) => p.then((r) => r && after?.());
  const live = isLive(sb.state), stopped = isStopped(sb.state);
  const own = sbAccess(sb) === 'owner', use = sbAccess(sb) !== 'view';
  const orgOwner = ctx.session.orgs.find((o) => o.id === sb.team?.id)?.role === 'owner';
  // Creator-only items stay in the menu, disabled with the reason.
  const mine = (it) => (own ? it : { ...it, disabled: true, title: CREATOR_ONLY, onClick: null });
  return [
    { label: 'Open', ico: 'chevron', onClick: () => ctx.nav(`/sandboxes/${sb.id}`) },
    live && use ? { label: 'Desktop', ico: 'monitor', onClick: () => ctx.nav(`/sandboxes/${sb.id}/desktop`) } : null,
    live && use ? { label: 'Browser', ico: 'globe', onClick: () => openBrowser(sb) } : null,
    live && use ? { label: 'Console', ico: 'terminal', onClick: () => ctx.nav(`/sandboxes/${sb.id}/console`) } : null,
    use ? { label: 'Copy SSH command', ico: 'copy', onClick: () => copy(`crowdvm ssh ${sb.id}`).then(() => toast('SSH command copied')) } : null,
    { label: 'AI usage…', ico: 'sparkles', onClick: () => aiUsageDialog(sb) },
    '-',
    live || isBusy(sb.state) ? { label: 'Stop', ico: 'stop', onClick: () => done(stopSandbox(sb)) } : null,
    use && (stopped || sb.state === 'error') ? { label: 'Resume…', ico: 'play', onClick: () => done(resumeDialog(sb)) } : null,
    mine({ label: 'Fork…', ico: 'fork', onClick: () => forkDialog(ctx, sb) }),
    use ? { label: 'Save as snapshot…', ico: 'camera', onClick: () => saveSnapshotDialog(sb) } : null,
    live ? mine({ label: 'Auto-stop…', ico: 'clock', onClick: () => done(extendDialog(sb)) }) : null,
    mine({ label: 'Rename…', ico: 'edit', onClick: () => done(renameDialog(sb)) }),
    sb.visibility && own ? { label: sb.visibility === 'shared' ? 'Make private' : 'Share with org', ico: sb.visibility === 'shared' ? 'lock' : 'share', onClick: () => done(toggleShare(sb)) } : null,
    use && (live || sb.state === 'error') ? { label: 'Recover', ico: 'lifebuoy', onClick: () => done(recoverSandbox(sb)) } : null,
    sb.state === 'archiving' && (own || orgOwner) ? { label: 'Force stop', ico: 'stop', danger: true, onClick: () => done(stopSandbox(sb, true)) } : null,
    '-',
    mine({ label: 'Delete…', ico: 'trash', danger: true, onClick: () => deleteSandbox(sb).then((r) => r && (after ? after(true) : null)) }),
  ];
}

export { isMissing };
