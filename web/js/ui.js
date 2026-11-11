// Shared UI pieces: toasts, dialogs, menus, chips, meters, tables.
import { h, icon, btn, copy, num } from './dom.js';
import { errText } from './api.js';

// ---------------- toasts ----------------
let toastHost;
export function toast(msg, kind = '') {
  if (!toastHost) { toastHost = h('div.toasts', { role: 'status', 'aria-live': 'polite' }); document.body.append(toastHost); }
  const el = h('div.toast', { class: kind },
    icon(kind === 'err' ? 'alert' : 'check'), h('div', msg),
    h('button.x', { 'aria-label': 'Dismiss', onclick: () => el.remove() }, icon('x', 'sm')));
  toastHost.append(el);
  setTimeout(() => el.remove(), kind === 'err' ? 8000 : 3500);
}
export const toastErr = (e) => toast(errText(e), 'err');

// ---------------- dialogs ----------------
// modal({title, desc, body, actions:[{label, kind, onClick(close, form) -> bool|Promise}] , wide, onClose})
export function modal({ title, desc, body, actions = [], wide = false, onClose, submit }) {
  const dlg = h('dialog.dlg', { class: wide ? 'wide' : '', 'aria-labelledby': 'dlg-t' });
  const form = h('form', { method: 'dialog', style: { display: 'contents' }, novalidate: true });
  const close = (v) => { if (dlg.open) dlg.close(v); };
  const err = h('div.notice.err', { hidden: true, role: 'alert' });
  const footBtns = actions.map((a) => {
    const b = btn(a.label, { kind: a.kind, ico: a.ico, type: a.submit ? 'submit' : 'button' });
    if (!a.submit) b.addEventListener('click', () => run(a, b));
    return b;
  });
  async function run(a, b) {
    err.hidden = true;
    if (!a.onClick) return close();
    const label = b.innerHTML;
    b.disabled = true;
    if (a.busy !== false) b.prepend(h('span.spinner'));
    try { const r = await a.onClick(close, form); if (r !== false) close(); }
    catch (e) { err.replaceChildren(icon('alert'), h('div', errText(e))); err.hidden = false; }
    finally { if (b.isConnected) { b.disabled = false; b.innerHTML = label; } }
  }
  form.addEventListener('submit', (e) => {
    e.preventDefault();
    const i = actions.findIndex((a) => a.submit);
    if (i >= 0) run(actions[i], footBtns[i]);
  });
  form.append(
    h('div.dlg-head', h('div', h('h2#dlg-t', title), desc ? h('p', desc) : null),
      btn('', { kind: 'ghost', size: 'sm', ico: 'x', title: 'Close', onClick: () => close() })),
    h('div.dlg-body', err, body),
    actions.length ? h('div.dlg-foot', footBtns) : null,
  );
  dlg.append(form);
  dlg.addEventListener('close', () => { onClose?.(dlg.returnValue); dlg.remove(); });
  dlg.addEventListener('click', (e) => { if (e.target === dlg) close(); }); // backdrop
  document.body.append(dlg);
  dlg.showModal();
  submit && setTimeout(() => form.querySelector('input,textarea,select')?.focus(), 0);
  return { dlg, form, close };
}

export function confirmDialog({ title, desc, confirm = 'Confirm', danger = false, typeToConfirm, body }) {
  return new Promise((resolve) => {
    let ok = false;
    const input = typeToConfirm ? h('input.input.mono', { autocomplete: 'off', 'aria-label': `Type ${typeToConfirm} to confirm` }) : null;
    const m = modal({
      title, desc,
      body: h('div.stack', body, input ? h('div.field', h('label', 'Type ', h('code', typeToConfirm), ' to confirm'), input) : null),
      actions: [
        { label: 'Cancel', onClick: () => true },
        { label: confirm, kind: danger ? 'danger solid' : 'primary', submit: true, busy: false, onClick: () => {
          if (input && input.value.trim() !== typeToConfirm) { input.setAttribute('aria-invalid', 'true'); input.focus(); return false; }
          ok = true; return true;
        } },
      ],
      onClose: () => resolve(ok),
    });
    (input || m.form.querySelector('.btn.primary,.btn.solid'))?.focus();
  });
}

// Show a one-time secret with copy button.
export function secretDialog({ title, desc, secret, extra }) {
  modal({
    title, desc,
    body: h('div.stack', h('div.notice.warn', icon('lock'), h('div', 'Copy it now. It will not be shown again.')), copyable(secret, { secret: true }), extra),
    actions: [{ label: 'Done', kind: 'primary', onClick: () => true }],
  });
}

// ---------------- menus ----------------
// menu(anchorButton, [{label, ico, onClick, danger, href, disabled, title} | '-' | {header}])
export function openMenu(anchor, items) {
  document.querySelector('.menu')?._close?.();
  const el = h('div.menu', { role: 'menu' });
  for (const it of items) {
    if (!it) continue;
    if (it === '-') { el.append(h('hr')); continue; }
    if (it.header) { el.append(h('div.label', it.header)); continue; }
    const tag = it.href ? 'a' : 'button';
    el.append(h(tag, { role: 'menuitem', href: it.href, class: it.danger ? 'danger' : '', disabled: it.disabled, title: it.title, type: it.href ? null : 'button',
      onclick: (e) => { close(); it.onClick?.(e); } }, it.ico ? icon(it.ico) : null, it.label));
  }
  document.body.append(el);
  const r = anchor.getBoundingClientRect(), mw = el.offsetWidth, mh = el.offsetHeight;
  let left = Math.min(r.right - mw, innerWidth - mw - 8); if (left < 8) left = Math.min(r.left, innerWidth - mw - 8);
  let top = r.bottom + 4; if (top + mh > innerHeight - 8) top = Math.max(8, r.top - mh - 4);
  Object.assign(el.style, { left: `${Math.max(8, left)}px`, top: `${top}px` });
  anchor.setAttribute('aria-expanded', 'true');
  const items$ = [...el.querySelectorAll('[role=menuitem]:not([disabled])')];
  items$[0]?.focus();
  function onKey(e) {
    const i = items$.indexOf(document.activeElement);
    if (e.key === 'Escape') { e.preventDefault(); close(); anchor.focus(); }
    else if (e.key === 'ArrowDown') { e.preventDefault(); items$[(i + 1) % items$.length]?.focus(); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); items$[(i - 1 + items$.length) % items$.length]?.focus(); }
    else if (e.key === 'Tab') close();
  }
  function onDown(e) { if (!el.contains(e.target) && e.target !== anchor && !anchor.contains(e.target)) close(); }
  function close() {
    el.remove(); anchor.setAttribute('aria-expanded', 'false');
    document.removeEventListener('keydown', onKey, true); document.removeEventListener('pointerdown', onDown, true);
    removeEventListener('resize', close);
  }
  el._close = close;
  document.addEventListener('keydown', onKey, true);
  document.addEventListener('pointerdown', onDown, true);
  addEventListener('resize', close);
}
export function menuButton(items, { label = '', title = 'More actions', ico = 'more', size = 'sm', kind = 'ghost' } = {}) {
  const b = btn(label, { kind, ico, title, size, attrs: { 'aria-haspopup': 'menu', 'aria-expanded': 'false' } });
  b.addEventListener('click', (e) => { e.stopPropagation(); openMenu(b, typeof items === 'function' ? items() : items); });
  return b;
}

// ---------------- chips & badges ----------------
const STATE = {
  init: ['info busy', 'starting'], provisioning: ['info busy', 'provisioning'], provisioned: ['info busy', 'starting'],
  cloning: ['info busy', 'cloning'], ready: ['ok', 'ready'], idle: ['ok', 'idle'], running: ['accent live', 'working'],
  archiving: ['warn busy', 'stopping'], archived: ['', 'stopped'], stopped: ['', 'stopped'], error: ['err', 'error'],
  cancelled: ['', 'cancelled'], saving: ['info busy', 'saving'], failed: ['err', 'failed'], completed: ['ok', 'completed'],
  active: ['ok', 'active'], revoked: ['', 'revoked'], expired: ['err', 'expired'], online: ['ok live', 'online'], offline: ['', 'offline'],
  pending: ['info busy', 'pending'], done: ['ok', 'done'], queued: ['info busy', 'queued'], finished: ['ok', 'finished'],
  interrupted: ['warn', 'interrupted'], degraded: ['warn', 'degraded'],
};
export function stateChip(state, label) {
  const [cls, text] = STATE[state] || ['', state || 'unknown'];
  return h('span.chip', { class: cls, title: state }, label || text);
}
export const isLive = (s) => ['ready', 'idle', 'running'].includes(s);
export const isBusy = (s) => ['init', 'provisioning', 'provisioned', 'cloning', 'archiving'].includes(s);
export const isStopped = (s) => s === 'archived' || s === 'stopped';

export function tierBadge(tier) {
  if (!tier) return h('span.tier.silver', 'unrated');
  return h('span.tier', { class: tier, title: `${tier} tier` }, icon('medal'), tier);
}
const LEVELS = { none: 0, low: 1, medium: 2, high: 3, extreme: 4 };
const CAT_ICONS = { cpu: 'cpu', memory: 'memory', disk: 'disk', network: 'network', gpu: 'gpu' };
export function catChip(cat, level = 'none') {
  const n = LEVELS[level] ?? 0;
  return h('span.cat', { class: level, title: `${cat}: ${level}` }, icon(CAT_ICONS[cat] || 'box', 'sm'), cat,
    h('span.lv', { 'aria-hidden': 'true' }, [1, 2, 3, 4].map((i) => h('i', { class: i <= n ? 'on' : '' }))),
    h('span.sr-only', level));
}

export function meter(label, used, total, fmt = num) {
  const pct = total > 0 ? Math.min(100, (used / total) * 100) : 0;
  return h('div.meter', { role: 'meter', 'aria-label': label, 'aria-valuemin': 0, 'aria-valuemax': total || 0, 'aria-valuenow': used || 0 },
    h('div.top', h('span', label), h('span', h('b', fmt(used || 0)), ' / ', fmt(total || 0))),
    h('div.bar', { class: pct >= 90 ? 'err' : pct >= 70 ? 'warn' : '' }, h('span', { style: { width: `${pct}%` } })));
}

export function copyable(text, { secret = false, label = 'Copy' } = {}) {
  const b = btn('', { kind: 'ghost', size: 'sm', ico: 'copy', title: label });
  b.addEventListener('click', async () => {
    if (await copy(text)) { b.replaceChildren(icon('check')); setTimeout(() => b.replaceChildren(icon('copy')), 1400); }
  });
  return h('div.copyable', { class: secret ? 'secret' : '' }, h('code', text), b);
}
export function copyBtn(text, title = 'Copy') {
  const b = btn('', { kind: 'ghost', size: 'sm', ico: 'copy', title });
  b.addEventListener('click', async (e) => { e.stopPropagation(); if (await copy(text)) toast('Copied to clipboard'); });
  return b;
}
export const idText = (id) => h('span.row', { style: { gap: '2px' } }, h('code.idlink', id), copyBtn(id, 'Copy id'));

// ---------------- layout bits ----------------
export function pageHead({ title, lede, actions, crumbs }) {
  return h('header.page-head',
    h('div.grow',
      crumbs ? h('nav.crumbs', { 'aria-label': 'Breadcrumb' }, crumbs.flatMap((c, i) => [i ? icon('chevron', 'sm') : null, c.href ? h('a', { href: c.href }, c.label) : h('span', c.label)])) : null,
      h('h1', title), lede ? h('p.lede', lede) : null),
    actions ? h('div.actions', actions) : null);
}
export function card({ title, sub, actions, body, foot, flush = false, cls = '' }) {
  return h('section.card', { class: cls },
    title ? h('div.card-head', h('div', h('h2', title), sub ? h('div.sub', sub) : null), actions ? h('div.row', actions) : null) : null,
    flush ? body : h('div.card-body', body),
    foot ? h('div.card-foot', foot) : null);
}
export function empty({ ico = 'box', title, text, action }) {
  return h('div.empty', h('div.ico', icon(ico, 'lg')), h('h3', title), text ? h('p', text) : null, action);
}
export const loading = (text = 'Loading…') => h('div.loading', h('span.spinner'), text);
export function errorBox(e, retry) {
  return h('div.notice.err', { role: 'alert' }, icon('alert'), h('div.grow', errText(e)), retry ? btn('Retry', { size: 'sm', onClick: retry }) : null);
}

// table({cols:[{label, cls, render(row)}], rows, onRow(row), empty})
export function table({ cols, rows, onRow, emptyEl, rowLabel }) {
  if (!rows.length && emptyEl) return emptyEl;
  return h('div.table-wrap', h('table.t',
    h('thead', h('tr', cols.map((c) => h('th', { class: c.cls, scope: 'col' }, c.label)))),
    h('tbody', rows.map((r) => {
      const tr = h('tr', { class: onRow ? 'clickable' : '' }, cols.map((c) => h('td', { class: c.cls }, c.render(r))));
      if (onRow) tr.addEventListener('click', (e) => { if (!e.target.closest('a,button,input,select,label')) onRow(r, e); });
      if (rowLabel) tr.setAttribute('aria-label', rowLabel(r));
      return tr;
    }))));
}

export function field(label, control, hint, { full = false, id } = {}) {
  id = id || control.id || `f-${Math.random().toString(36).slice(2, 8)}`;
  const target = control.matches?.('input,select,textarea') ? control : control.querySelector?.('input,select,textarea');
  if (target && !target.id) target.id = id;
  return h('div.field', { class: full ? 'full' : '' },
    h('label', { for: target?.id || id }, label), control, hint ? h('div.hint', hint) : null);
}
export function checkbox(name, title, desc, checked = false) {
  return h('label.check', h('input', { type: 'checkbox', name, checked }), h('div', h('div.t', title), desc ? h('div.d', desc) : null));
}

// Segmented single-choice control. Returns element with .value getter.
export function segmented(options, value, onChange, label) {
  const el = h('div.seg', { role: 'radiogroup', 'aria-label': label });
  const draw = () => el.replaceChildren(...options.map(([v, l]) => h('button', {
    type: 'button', role: 'radio', 'aria-checked': String(v === value), tabindex: v === value ? '0' : '-1',
    onclick: () => { value = v; draw(); onChange?.(v); },
  }, l)));
  el.addEventListener('keydown', (e) => {
    const i = options.findIndex(([v]) => v === value);
    const d = e.key === 'ArrowRight' || e.key === 'ArrowDown' ? 1 : e.key === 'ArrowLeft' || e.key === 'ArrowUp' ? -1 : 0;
    if (!d) return;
    e.preventDefault(); value = options[(i + d + options.length) % options.length][0]; draw(); onChange?.(value);
    el.querySelector('[aria-checked=true]')?.focus();
  });
  draw();
  Object.defineProperty(el, 'value', { get: () => value });
  return el;
}

export function tabs(items, current, base) {
  return h('nav.tabs', { 'aria-label': 'Sections' }, items.map(([key, label, ico]) =>
    h('a', { href: key ? `${base}/${key}` : base, 'aria-current': key === current ? 'page' : null }, ico ? icon(ico) : null, label)));
}

// ---------------- brand ----------------
const LOGO = () => {
  const t = document.createElement('template');
  t.innerHTML = '<svg class="logo" viewBox="0 0 32 32" aria-hidden="true"><rect width="32" height="32" rx="8" fill="var(--accent)"/><rect x="7" y="7" width="8" height="8" rx="2" fill="var(--accent-fg)"/><rect x="17" y="7" width="8" height="8" rx="2" fill="var(--accent-fg)" fill-opacity=".55"/><rect x="7" y="17" width="8" height="8" rx="2" fill="var(--accent-fg)" fill-opacity=".55"/><rect x="17.75" y="17.75" width="6.5" height="6.5" rx="1.5" fill="none" stroke="var(--accent-fg)" stroke-width="1.5"/></svg>';
  return t.content.firstChild;
};
export const brand = (href = '/') => h('a.brand', { href }, LOGO(), 'CrowdVM');
