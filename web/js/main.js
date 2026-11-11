// App shell: session bootstrap, routing, sidebar, keyboard shortcuts.
import { h, icon, btn, initials, num } from './dom.js';
import { api, auth, setUnauthorizedHandler, setOrgScope, errText } from './api.js';
import { openMenu, modal, toastErr, toast, brand } from './ui.js';


const ADMIN_NAV = ['Server', [['/admin', 'Admin', 'shield', 'x']]];
const NAV = [
  ['Compute', [
    ['/sandboxes', 'Sandboxes', 'box', 's'],
    ['/snapshots', 'Saved snapshots', 'camera', 'p'],
    ['/nodes', 'Nodes', 'server', 'n'],
  ]],
  ['Configure', [
    ['/environments', 'Environments', 'layers', 'e'],
    ['/ai', 'AI credentials', 'sparkles', 'a'],
    ['/keys', 'API keys', 'key', 'k'],
    ['/webhooks', 'Webhooks', 'webhook', 'w'],
    ['/telemetry', 'Telemetry', 'activity', 't'],
    ['/ci', 'CI runners', 'git', 'i'],
    ['/api-explorer', 'API explorer', 'braces', 'r'],
  ]],
  ['Account', [
    ['/org', 'Organization', 'users', 'o'],
    ['/credits', 'Credits & usage', 'coins', 'c'],
    ['/account', 'Account', 'user', 'u'],
  ]],
];

// [pattern, module, {public, bare}]
const ROUTES = [
  [/^\/login$/, 'auth', { public: true, bare: true }],
  [/^\/signup$/, 'auth', { public: true, bare: true }],
  [/^\/device$/, 'device', { bare: true }],
  [/^\/invite$/, 'invite', { bare: true }],
  [/^\/desktop\/(?<id>[\w-]+)$/, 'desktop', { public: true, bare: true }],
  [/^\/sandboxes$/, 'sandboxes'],
  [/^\/sandboxes\/(?<id>[\w-]+)(?:\/(?<tab>[\w-]+))?$/, 'sandbox'],
  [/^\/snapshots$/, 'snapshots'],
  [/^\/nodes$/, 'nodes'],
  [/^\/environments$/, 'environments'],
  [/^\/environments\/(?<id>[^/]+)$/, 'environment'],
  [/^\/ai$/, 'ai'],
  [/^\/keys$/, 'keys'],
  [/^\/webhooks$/, 'webhooks'],
  [/^\/telemetry$/, 'telemetry'],
  [/^\/ci$/, 'ci'],
  [/^\/api-explorer$/, 'explorer'],
  [/^\/org$/, 'org'],
  [/^\/credits$/, 'credits'],
  [/^\/account$/, 'account'],
  [/^\/admin$/, 'admin'],
];

// active = the account's active wallet (what the CLI and API keys bill).
// view = the organization the dashboard shows and creates in; kept in this browser only.
export const session = { me: null, orgs: [], active: null, view: null, credits: null, notices: [] };
const VIEW_KEY = 'crowdvm.view';
const root = document.getElementById('root');
let shell = null, current = null;

export function nav(path, { replace = false } = {}) {
  if (path === location.pathname + location.search) return render();
  history[replace ? 'replaceState' : 'pushState'](null, '', path);
  render();
}

setUnauthorizedHandler(() => {
  session.me = null;
  if (!location.pathname.match(/^\/(login|signup|desktop)/)) {
    nav(`/login?next=${encodeURIComponent(location.pathname + location.search)}`, { replace: true });
  }
});

async function loadSession() {
  try {
    const r = await auth.me();
    session.me = r.user || r;
  } catch { session.me = null; }
}

export async function refreshShell() {
  if (!session.me) return;
  const [orgs, credits, me, notices] = await Promise.allSettled([api.get('/orgs'), api.get('/credits'), auth.me(), api.get('/notices')]);
  session.notices = notices.status === 'fulfilled' ? notices.value.notices || [] : [];
  if (me.status === 'fulfilled' && (me.value.user || me.value).id) session.me = me.value.user || me.value;
  if (orgs.status === 'fulfilled') {
    session.orgs = orgs.value.orgs || [];
    session.active = session.orgs.find((o) => o.active) || session.orgs[0] || null;
    let stored = null;
    try { stored = localStorage.getItem(VIEW_KEY); } catch { /* private mode */ }
    applyView(session.orgs.find((o) => o.id === stored) || session.active);
  }
  session.credits = credits.status === 'fulfilled' ? credits.value : null;
  if (shell) drawSidebarDynamic();
}

function applyView(o) {
  session.view = o || null;
  setOrgScope(!o ? null : o.type === 'personal' ? 'personal' : o.id);
}

// Change what the dashboard views. Never touches the active wallet.
export function setView(id) {
  const o = session.orgs.find((x) => x.id === id);
  if (!o) return;
  try { localStorage.setItem(VIEW_KEY, o.id); } catch { /* private mode */ }
  applyView(o);
  if (shell) drawSidebarDynamic();
  render();
}

// Sidebar "CLI and API keys bill … Change": the only control that sets the active wallet.
function changeWallet(anchor) {
  openMenu(anchor, [
    { header: 'CLI and API keys bill' },
    ...session.orgs.map((o) => ({ label: `${orgLabel(o)}${o.active ? '  ✓' : ''}`, onClick: async () => {
      if (o.active) return;
      try { await api.patch('/orgs/active', { org: o.type === 'personal' ? null : o.id }); await refreshShell(); toast(`The CLI and API keys now bill ${orgLabel(o)}`); }
      catch (e) { toastErr(e); }
    } })),
  ]);
}
const orgLabel = (o) => (o.type === 'personal' ? `${o.name} (personal)` : o.name);

// ---------------- theme ----------------
export function getTheme() { try { return localStorage.getItem('crowdvm.theme') || 'system'; } catch { return 'system'; } }
export function setTheme(t) {
  try { t === 'system' ? localStorage.removeItem('crowdvm.theme') : localStorage.setItem('crowdvm.theme', t); } catch { /* private mode */ }
  if (t === 'system') delete document.documentElement.dataset.theme; else document.documentElement.dataset.theme = t;
}

// ---------------- shell ----------------
function buildShell() {
  const sidebar = h('aside.sidebar', { id: 'sidebar', 'aria-label': 'Main navigation' });
  const main = h('main.main', { id: 'main', tabindex: '-1' });
  const topbar = h('div.topbar',
    btn('', { kind: 'ghost', ico: 'menu', title: 'Open navigation', onClick: () => toggleNav(true), attrs: { 'aria-controls': 'sidebar' } }),
    brand('/sandboxes'));
  const notices = h('div.shell-notices', { role: 'status', 'aria-live': 'polite' });
  const app = h('div.app', h('a.skip-link', { href: '#main' }, 'Skip to content'), sidebar, h('div.main', topbar, notices, main));
  root.replaceChildren(app);
  shell = { app, sidebar, main, notices, orgSlot: h('div.org-switch'), footSlot: h('div.sidebar-foot'), counts: {} };
  const navEl = h('nav.nav', [...NAV, session.me?.isAdmin ? ADMIN_NAV : null].filter(Boolean).map(([group, items]) => [
    h('div.nav-label', group),
    items.map(([href, label, ico]) => h('a', { href, onclick: () => toggleNav(false) }, icon(ico), h('span', label), shell.counts[href] = h('span.count'))),
  ]));
  sidebar.append(brand('/sandboxes'), shell.orgSlot, navEl, shell.footSlot);
  drawSidebarDynamic();
}

function toggleNav(open) {
  if (!shell) return;
  shell.app.classList.toggle('nav-open', open);
  shell.app.querySelector('.scrim')?.remove();
  if (open) {
    shell.app.append(h('div.scrim', { onclick: () => toggleNav(false) }));
    shell.sidebar.querySelector('a[aria-current=page], a')?.focus();
  }
}

// Account-wide alerts above every page: closed account, credits running out.
function drawNotices() {
  const me = session.me || {}, w = session.credits?.wallet || session.credits || {};
  const bal = w.balanceCredits ?? w.balance, enforced = w.enforced ?? session.credits?.enforced;
  const out = [];
  if (me.closed) {
    out.push(h('div.notice.err', icon('alert'), h('div', h('b', 'This account is closed. '), 'Sandboxes are stopped and changes are refused.',
      me.purgeAt ? ` Its data is purged on ${new Date(me.purgeAt).toLocaleDateString()}. ` : ' ', h('a', { href: '/account' }, 'Reopen it'))));
  }
  // Server notices (credits, failing stops, node loss, disk full…), newest first, dismissable.
  const LV = { error: 'err', warning: 'warn' };
  const shown = session.notices.slice(0, 3);
  for (const n of shown) {
    out.push(h('div.notice', { class: LV[n.level] || '' }, icon(n.level === 'error' || n.level === 'warning' ? 'alert' : 'info'),
      h('div.grow', n.title ? h('b', n.title, ' ') : null, n.message, n.sandboxId ? [' ', h('a', { href: `/sandboxes/${n.sandboxId}` }, 'Open sandbox')] : null),
      btn('', { kind: 'ghost', size: 'sm', ico: 'x', title: 'Dismiss', onClick: () => dismiss(n.id) })));
  }
  if (session.notices.length > shown.length) {
    out.push(h('div.row.end.small', h('span.muted', `${session.notices.length - shown.length} more notices`), btn('Dismiss all', { kind: 'ghost', size: 'sm', onClick: () => dismiss() })));
  }
  if (enforced && bal != null && bal < 2 && !session.notices.some((n) => /credit|grace|balance|billing/.test(n.kind))) {
    out.push(h('div.notice', { class: bal <= 0 ? 'err' : 'warn' }, icon('coins'), h('div',
      bal <= 0 ? 'You are out of credits: new sandboxes do not start and running ones are stopped. ' : `Credits are low (${num(bal, 1)} left, about ${num(bal, 1)} hours of a default sandbox). `,
      h('a', { href: '/credits' }, 'Credits & usage'))));
  }
  shell.notices.replaceChildren(...out);
}

async function dismiss(id) {
  try {
    await api.post(id ? `/notices/${encodeURIComponent(id)}/dismiss` : '/notices/dismiss-all', {});
    session.notices = id ? session.notices.filter((n) => n.id !== id) : [];
    drawNotices();
  } catch (e) { toastErr(e); }
}

function drawSidebarDynamic() {
  drawNotices();
  const { orgSlot, footSlot } = shell;
  orgSlot.replaceChildren();
  if (session.orgs.length > 1) {
    const sel = h('select.select.sm', { id: 'org-view', title: 'What the dashboard shows, and where the sandboxes and keys you create here bill' },
      session.orgs.map((o) => h('option', { value: o.id, selected: o.id === session.view?.id }, orgLabel(o))));
    sel.addEventListener('change', () => setView(sel.value));
    const change = h('button.link', { type: 'button', 'aria-haspopup': 'menu', 'aria-expanded': 'false', onclick: () => changeWallet(change) }, 'Change');
    orgSlot.append(h('label.org-label', { for: 'org-view' }, 'Viewing'), sel,
      h('div.org-wallet', { title: 'The active wallet: what the CLI and API keys bill when a request names no organization' },
        'CLI and API keys bill: ', h('b.truncate', session.active ? session.active.name : '—'), ' · ', change));
  }
  const me = session.me || {};
  const name = me.displayName || me.login || me.username || 'You';
  const c = session.credits;
  const userBtn = btn('', { kind: 'ghost', attrs: { 'aria-haspopup': 'menu', 'aria-expanded': 'false', class: 'btn ghost user-btn' } });
  userBtn.replaceChildren(h('span.avatar', initials(name)), h('span.truncate', { style: { textAlign: 'left', flex: 1 } }, name), icon('down', 'sm'));
  userBtn.addEventListener('click', () => {
    const t = getTheme();
    openMenu(userBtn, [
      { header: me.login || me.username || '' },
      { label: 'Account', ico: 'user', onClick: () => nav('/account') },
      { label: 'Keyboard shortcuts', ico: 'keyboard', onClick: showShortcuts },
      '-',
      { header: 'Theme' },
      ...[['system', 'System', 'laptop'], ['light', 'Light', 'sun'], ['dark', 'Dark', 'moon']].map(([v, l, i]) =>
        ({ label: `${l}${t === v ? '  ✓' : ''}`, ico: i, onClick: () => setTheme(v) })),
      '-',
      { label: 'Sign out', ico: 'logout', danger: true, onClick: logout },
    ]);
  });
  footSlot.replaceChildren(
    c ? h('a.credit-pill', { href: '/credits', title: 'Credit balance' }, icon('coins'), h('span', h('b', num(c.balance ?? c.wallet?.balanceCredits, 1)), ' credits'),
      (c.enforced ?? c.wallet?.enforced) === false ? h('span.faint', { style: { marginLeft: 'auto' } }, 'not enforced') : null) : null,
    userBtn);
}

async function logout() {
  try { await auth.logout(); } catch { /* ignore */ }
  session.me = null; shell = null;
  nav('/login', { replace: true });
}

function setActiveNav() {
  if (!shell) return;
  for (const a of shell.sidebar.querySelectorAll('.nav a')) {
    const on = location.pathname === a.getAttribute('href') || location.pathname.startsWith(a.getAttribute('href') + '/');
    a.toggleAttribute('aria-current', on);
    if (on) a.setAttribute('aria-current', 'page');
  }
}
export function setNavCount(href, n) { if (shell?.counts[href]) shell.counts[href].textContent = n ? String(n) : ''; }

// ---------------- router ----------------
async function render() {
  const path = location.pathname.replace(/\/+$/, '') || '/';
  if (path === '/' && location.hash.startsWith('#/')) return nav(location.hash.slice(1), { replace: true }); // old /#/path links (e.g. in emails)
  if (path === '/') return nav(session.me ? '/sandboxes' : '/login', { replace: true });
  if (path === '/dashboard') {
    const q = new URLSearchParams(location.search), tab = q.get('tab');
    q.delete('tab');
    return nav(`/${tab && /^[\w-]+$/.test(tab) ? tab : 'sandboxes'}${q.size ? `?${q}` : ''}`, { replace: true });
  }
  let match = null, mod = null, opts = {};
  for (const [re, m, o = {}] of ROUTES) { const x = re.exec(path); if (x) { match = x; mod = m; opts = o; break; } }

  current?.abort();
  const ctl = new AbortController();
  current = ctl;

  if (!opts.public && !session.me) {
    return nav(`/login?next=${encodeURIComponent(location.pathname + location.search)}`, { replace: true });
  }
  if (session.me && (path === '/login' || path === '/signup')) return nav(new URLSearchParams(location.search).get('next') || '/sandboxes', { replace: true });

  let host;
  if (opts.bare || !mod) {
    shell = null;
    host = h('div');
    root.replaceChildren(host);
  } else {
    // The first page waits for the org list: its requests must carry the viewed organization.
    if (!shell) { buildShell(); await refreshShell(); if (ctl.signal.aborted) return; }
    setActiveNav();
    host = h('div.content');
    shell.main.replaceChildren(host);
    toggleNav(false);
  }
  const ctx = {
    root: host, signal: ctl.signal, params: match?.groups || {}, query: new URLSearchParams(location.search), path,
    nav, session, refreshShell, setNavCount, setView,
    title: (t) => { document.title = t ? `${t} · CrowdVM` : 'CrowdVM'; },
    wide: (on = true) => host.classList.toggle('wide', on),
  };
  if (!mod) {
    ctx.title('Not found');
    host.className = 'content';
    host.append(h('div.empty', { style: { minHeight: '60vh', justifyContent: 'center' } }, h('div.ico', icon('alert', 'lg')), h('h3', 'Page not found'),
      h('p', `Nothing lives at ${path}.`), h('a.btn', { href: '/sandboxes' }, 'Go to sandboxes')));
    return;
  }
  try {
    const page = await import(`./pages/${mod}.js`);
    if (ctl.signal.aborted) return;
    await page.default(ctx);
    if (!opts.bare && shell && document.activeElement === document.body) shell.main.focus({ preventScroll: true });
  } catch (e) {
    if (e.name === 'AbortError' || ctl.signal.aborted) return;
    console.error(e);
    host.replaceChildren(h('div.notice.err', { role: 'alert' }, icon('alert'), h('div', errText(e))));
  }
}

document.addEventListener('click', (e) => {
  const a = e.target.closest('a[href]');
  if (!a || e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
  if (a.target || a.hasAttribute('download') || a.dataset.native != null) return;
  const url = new URL(a.href, location.href);
  if (url.origin !== location.origin || url.pathname.startsWith('/api/') || url.pathname.startsWith('/auth/')) return;
  if (url.pathname === location.pathname && url.hash) return;
  e.preventDefault();
  nav(url.pathname + url.search);
});
addEventListener('popstate', render);

// ---------------- keyboard ----------------
let gPending = 0;
function typing(t) { return t.closest('input,textarea,select,[contenteditable=""],[contenteditable=true],.screen'); }
document.addEventListener('keydown', (e) => {
  if (e.metaKey || e.ctrlKey || e.altKey || typing(e.target) || document.querySelector('dialog[open]') || !shell) return;
  if (gPending && Date.now() - gPending < 1200) {
    gPending = 0;
    const item = NAV.flatMap(([, i]) => i).find((i) => i[3] === e.key);
    if (item) { e.preventDefault(); nav(item[0]); }
    return;
  }
  if (e.key === 'g') { gPending = Date.now(); return; }
  if (e.key === '/') { const s = document.querySelector('[data-search]'); if (s) { e.preventDefault(); s.focus(); } }
  else if (e.key === '?') { e.preventDefault(); showShortcuts(); }
  else if (e.key === 'c') { const b = document.querySelector('[data-create]'); if (b) { e.preventDefault(); b.click(); } }
});

function showShortcuts() {
  const row = (keys, what) => h('tr', h('td', keys.map((k) => h('span.kbd', k)).reduce((a, k, i) => (i ? [...a, ' ', k] : [k]), [])), h('td', what));
  modal({
    title: 'Keyboard shortcuts',
    body: h('table.t', h('tbody',
      row(['c'], 'Create (sandbox, key, …) on the current page'),
      row(['/'], 'Focus search'),
      row(['?'], 'Show this help'),
      NAV.flatMap(([, items]) => items).map(([, label, , k]) => row(['g', k], `Go to ${label}`)),
      row(['Esc'], 'Close dialog or menu'))),
    actions: [{ label: 'Close', onClick: () => true }],
  });
}

// ---------------- boot ----------------
await loadSession();
render();
if (session.me) setInterval(() => { if (!document.hidden && shell) refreshShell(); }, 30000);
