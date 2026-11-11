// Account: profile, connections, password, data & privacy, close/erase, appearance, CLI setup.
import { h, icon, btn, initials, time, dateTime, plural } from '../dom.js';
import { api, auth, request, errText } from '../api.js';
import { pageHead, card, field, toast, toastErr, copyable, segmented, checkbox, confirmDialog, loading, errorBox, menuButton } from '../ui.js';

const METHODS = { password: 'Username & password', email: 'Email sign-in code', agent: 'Agent identity' };

export default async function (ctx) {
  ctx.title('Account');
  const page = h('div');
  ctx.root.append(pageHead({ title: 'Account' }), page);

  async function reloadMe() {
    const r = await auth.me();
    ctx.session.me = r.user || r;
    ctx.refreshShell();
    draw();
  }

  function draw() {
    const me = ctx.session.me || {};
    const closed = !!me.closed;
    page.replaceChildren(h('div.stack.loose',
      closed ? closedBanner(me) : null,
      h('div.grid.side',
        h('div.stack.loose', profileCard(me, closed), passwordCard(closed), privacyCard(closed), cliCard(), dangerCard(me, closed)),
        h('div.stack.loose', appearanceCard(), connectionsCard(me, closed), sessionCard()))));
  }

  // ---------------- closed state ----------------
  function closedBanner(me) {
    return h('div.notice.warn', { role: 'status' }, icon('lock'), h('div.grow.stack.tight',
      h('b', 'This account is closed.'),
      h('div', 'Sandboxes were stopped, API keys revoked and changes are blocked. ',
        me.purgedAt ? 'Its sandbox data was purged.' : ['Reopen it before ', h('b', dateTime(me.purgeAt)), ' to keep its sandbox data; after that it is purged for good.']),
      h('div', 'Reopening restores access; revoked API keys stay revoked.'),
      h('div', btn('Reopen account', { kind: 'primary', size: 'sm', ico: 'refresh', onClick: reopen }))));
  }
  async function reopen() {
    try {
      const r = await api.post('/account/reopen', {});
      toast(r.purged ? 'Account reopened. Its sandbox data was already purged.' : 'Account reopened');
      await reloadMe();
    } catch (e) { toastErr(e); }
  }

  // ---------------- profile ----------------
  function profileCard(me, closed) {
    const name = me.displayName || me.login || me.username;
    const display = h('input.input', { value: me.displayName || '', placeholder: name, maxlength: 80, disabled: closed });
    const save = btn('Save', { size: 'sm', kind: 'primary', disabled: closed, onClick: async () => {
      try { const r = await request('PATCH', '/auth/me', { displayName: display.value.trim() }); ctx.session.me = r.user || { ...me, displayName: display.value.trim() }; toast('Profile saved'); ctx.refreshShell(); }
      catch (e) { toastErr(e); }
    } });
    return card({ title: 'Profile', body: h('div.stack',
      h('div.row', { style: { gap: '14px' } }, h('span.avatar.lg', initials(name)), h('div', h('b', name), h('div.faint.small', `@${me.login || me.username}${me.createdAt ? ` · joined ${time(me.createdAt).textContent}` : ''}`), me.isAdmin ? h('span.chip.accent.plain', { style: { marginTop: '4px' } }, 'server admin') : null)),
      field('Display name', display, 'Shown to organization members.'),
      h('div.row.end', save)) });
  }

  function passwordCard(closed) {
    const cur = h('input.input', { type: 'password', autocomplete: 'current-password', disabled: closed });
    const nw = h('input.input', { type: 'password', autocomplete: 'new-password', minlength: 8, disabled: closed });
    const msg = h('div', { role: 'status' });
    const form = h('form.stack', { novalidate: true }, h('div.fields', field('Current password', cur), field('New password', nw, 'At least 8 characters.')), msg, h('div.row.end', btn('Change password', { size: 'sm', type: 'submit', disabled: closed })));
    form.addEventListener('submit', async (e) => {
      e.preventDefault();
      msg.replaceChildren();
      if (nw.value.length < 8) { msg.append(h('div.notice.err', icon('alert'), h('div', 'Use at least 8 characters.'))); return; }
      try { await request('POST', '/auth/password', { currentPassword: cur.value, newPassword: nw.value }); cur.value = nw.value = ''; toast('Password changed'); }
      catch (err) { msg.append(h('div.notice.err', icon('alert'), h('div', err.status === 401 ? 'The current password is wrong.' : errText(err)))); }
    });
    return card({ title: 'Password', body: form });
  }

  // ---------------- connections ----------------
  function connectionsCard(me, closed) {
    const box = h('div', loading());
    const load = async () => {
      let r;
      try { r = await api.get('/account/connections', undefined, { signal: ctx.signal }); }
      catch (e) {
        if (e.name === 'AbortError') return;
        // Older servers: only the sign-in methods on /auth/me.
        r = { connections: (me.connectionMethods || [me.connectionMethod]).filter(Boolean).map((m) => ({ method: m, primary: m === me.connectionMethod, address: m === 'email' ? me.email : null })), legacy: true };
      }
      draw(r);
    };
    const act = (p, msg) => p.then(async () => { toast(msg); try { const x = await request('GET', '/auth/me'); ctx.session.me = me = x.user || x; } catch { /* keep */ } load(); }, toastErr);
    const draw = (r) => {
      const conns = r.connections || [];
      const email = h('input.input', { type: 'email', value: me.email || '', placeholder: 'you@example.com', autocomplete: 'email', disabled: closed });
      const notify = h('input.input', { type: 'email', value: r.notificationEmail && r.notificationEmail !== me.email ? r.notificationEmail : '', placeholder: me.email || 'you@example.com', disabled: closed });
      box.replaceChildren(h('div.stack',
        h('ul.stack.tight', { style: { margin: 0, padding: 0, listStyle: 'none' } }, conns.map((c) => h('li.row', { style: { gap: '8px' } },
          icon(c.method === 'email' ? 'send' : c.method === 'agent' ? 'bot' : 'key'),
          h('span.grow.truncate', METHODS[c.method] || c.method, c.address || c.login ? h('span.faint.small', ` · ${c.address || c.login}`) : null),
          c.primary && conns.length > 1 ? h('span.chip.plain', 'primary') : null,
          !r.legacy && !closed && conns.length > 1 ? menuButton([
            !c.primary ? { label: 'Make primary', ico: 'check', onClick: () => act(api.put('/account/connections/primary', { method: c.method }), 'Primary sign-in changed') } : null,
            { label: 'Remove…', ico: 'trash', danger: true, onClick: async () => {
              if (await confirmDialog({ title: `Remove ${METHODS[c.method] || c.method}?`, desc: 'You can no longer sign in this way. At least one way to sign in always remains.', confirm: 'Remove', danger: true }))
                act(api.del(`/account/connections/${encodeURIComponent(c.method)}`), 'Sign-in method removed');
            } },
          ], { title: `Actions for ${METHODS[c.method] || c.method}` }) : null))),
        field('Email', email, 'Adds email sign-in codes and is the default address for notices.'),
        h('div.row.end', btn('Save email', { size: 'sm', disabled: closed, onClick: () => {
          const v = email.value.trim();
          if (!v && conns.length === 1 && conns[0].method === 'email') return toast('You sign in with email codes only, so an address is required.', 'err');
          act(request('PATCH', '/auth/me', { email: v }), v ? 'Email saved' : 'Email removed');
        } })),
        r.legacy ? null : field('Notices go to', notify, 'Optional. Another address for alerts (credits, failing stops, full disks). Empty = the email above.'),
        r.legacy ? null : h('div.row.end', btn('Save notice address', { size: 'sm', disabled: closed, onClick: () => act(api.patch('/account-settings', { notifyEmail: notify.value.trim() }), 'Notice address saved') }))));
    };
    load();
    return card({ title: 'Connections', sub: 'How you sign in and where notices go.', body: box });
  }

  // ---------------- data & privacy ----------------
  function privacyCard(closed) {
    const box = h('div', loading());
    const loadPolicy = async () => {
      try { drawPolicy(await api.get('/account/data-retention', undefined, { signal: ctx.signal })); }
      catch (e) { if (e.name !== 'AbortError') box.replaceChildren(errorBox(e, loadPolicy)); }
    };
    const since = (on, at) => (on && at ? h('span.faint', ' · on since ', time(at)) : null);
    function drawPolicy(p) {
      const zdr = checkbox('zdr', 'Zero data retention', ['Stopping a sandbox deletes it, including auto-stops and stops for missing credits. No saved snapshots.', since(p.enabled, p.enabledAt)], p.enabled);
      const snaps = checkbox('snapshotsOff', 'Snapshots off', ['New sandboxes keep no disk when they stop, so they cannot be resumed or forked.', since(p.snapshotsOff, p.snapshotsOffAt)], p.snapshotsOff);
      const mon = checkbox('monitoringOff', 'Monitoring off', ['New sandboxes keep no command history, process list or metrics, and send no telemetry metrics.', since(p.monitoringOff, p.monitoringOffAt)], p.monitoringOff);
      for (const [el, key] of [[zdr, 'enabled'], [snaps, 'snapshotsOff'], [mon, 'monitoringOff']]) {
        const input = el.querySelector('input');
        input.disabled = closed;
        input.addEventListener('change', () => change(input, key));
      }
      box.replaceChildren(h('div.stack', zdr, snaps, mon,
        h('p.small.faint', 'Snapshots off and monitoring off apply to sandboxes created from now on.')));
    }
    async function change(input, key) {
      const on = input.checked;
      const body = { [key]: on };
      if (key === 'enabled' && on) {
        const ok = await confirmDialog({
          title: 'Turn on zero data retention?', danger: true, confirm: 'Turn on and delete', typeToConfirm: 'delete archived sandbox data',
          body: h('div.stack.tight.small',
            h('p', 'This deletes data now and cannot be undone:'),
            h('ul.stack.tight', { style: { margin: 0, paddingLeft: '18px' } },
              h('li', 'Every stopped sandbox is deleted, with its disk.'),
              h('li', 'Every saved snapshot is deleted.'),
              h('li', 'From now on a sandbox is deleted as soon as it stops, and saved snapshots cannot be created.')),
            h('p.muted', 'Turning it off later does not bring anything back.')),
        });
        if (!ok) { input.checked = false; return; }
        body.confirmation = 'delete archived sandbox data';
      }
      input.disabled = true;
      try {
        const r = await api.patch('/account/data-retention', body);
        toast(key === 'enabled' && on ? `Zero data retention on${r.queuedSandboxes ? `: deleting ${plural(r.queuedSandboxes, 'stopped sandbox', 'stopped sandboxes')}` : ''}` : 'Saved');
        drawPolicy(r);
      } catch (e) { input.checked = !on; input.disabled = false; toastErr(e); }
    }
    loadPolicy();
    return card({ title: 'Data & privacy', sub: 'What the server keeps about your sandboxes.', body: box });
  }

  // ---------------- close / erase ----------------
  function dangerCard(me, closed) {
    const c = card({ title: closed ? 'Erase account' : 'Close or erase account', body: h('div.stack',
      closed ? null : [h('div.row.between.wrap', { style: { gap: '12px' } }, h('p.small.muted.grow', { style: { minWidth: '200px' } }, 'Stop everything and lock the account. You can reopen it within 30 days.'),
        btn('Close account…', { kind: 'danger', ico: 'lock', onClick: close })),
      h('hr', { style: { border: 0, borderTop: '1px solid var(--border)', margin: 0 } })],
      h('div.row.between.wrap', { style: { gap: '12px' } }, h('p.small.muted.grow', { style: { minWidth: '200px' } }, 'Delete all your data and your identity now. This cannot be undone.'),
        btn('Erase account…', { kind: 'danger', ico: 'trash', onClick: erase }))) });
    c.style.borderColor = 'color-mix(in srgb, var(--err) 35%, var(--border))';
    return c;
  }
  async function close() {
    const ok = await confirmDialog({
      title: 'Close your account?', danger: true, confirm: 'Close account', typeToConfirm: 'close my account',
      body: h('div.stack.tight.small',
        h('ul.stack.tight', { style: { margin: 0, paddingLeft: '18px' } },
          h('li', 'Running sandboxes are stopped; their disks are saved.'),
          h('li', 'Every API key is revoked, also for the CLI. Reopening does not restore them.'),
          h('li', 'Other browser and CLI sessions are signed out.'),
          h('li', 'Nothing can be created or changed while it is closed.')),
        h('p.muted', 'Reopen it here within 30 days. After that its sandbox data is purged for good.')),
    });
    if (!ok) return;
    try { const r = await api.post('/account/close', { confirmation: 'close my account' }); toast(`Account closed${r.stoppedSandboxes ? `, stopping ${plural(r.stoppedSandboxes, 'sandbox', 'sandboxes')}` : ''}`); await reloadMe(); }
    catch (e) { toastErr(e); }
  }
  async function erase() {
    const ok = await confirmDialog({
      title: 'Erase your account?', danger: true, confirm: 'Erase everything', typeToConfirm: 'erase my account',
      body: h('div.stack.tight.small',
        h('p', 'Right now and for good:'),
        h('ul.stack.tight', { style: { margin: 0, paddingLeft: '18px' } },
          h('li', 'Every sandbox, disk and saved snapshot is deleted.'),
          h('li', 'Webhooks, telemetry export and API keys are removed.'),
          h('li', 'Your username, email, name and password are wiped and you leave every organization.'),
          h('li', 'You are signed out everywhere and cannot sign in again.')),
        h('p.muted', 'Credit ledger and audit records stay, without your identity.')),
    });
    if (!ok) return;
    try { await api.post('/account/erase', { confirmation: 'erase my account' }); ctx.session.me = null; location.href = '/login'; }
    catch (e) { toastErr(e); }
  }

  // ---------------- misc ----------------
  function cliCard() {
    return card({ title: 'Use the CLI', sub: 'Sign in once; the CLI gets its own API key after you approve it in this browser.', body: h('div.stack',
      copyable(`crowdvm login --server ${location.origin}`),
      h('p.small.muted', 'Agents and scripts can use an ', h('a', { href: '/keys' }, 'API key'), ' instead: ', h('code', 'CROWDVM_API_KEY'), ' and ', h('code', `CROWDVM_API_BASE=${location.origin}/api/v1`), '.')) });
  }
  function appearanceCard() {
    const theme = (() => { try { return localStorage.getItem('crowdvm.theme') || 'system'; } catch { return 'system'; } })();
    const seg = segmented([['system', 'System'], ['light', 'Light'], ['dark', 'Dark']], theme, (t) => {
      try { t === 'system' ? localStorage.removeItem('crowdvm.theme') : localStorage.setItem('crowdvm.theme', t); } catch { /* blocked */ }
      if (t === 'system') delete document.documentElement.dataset.theme; else document.documentElement.dataset.theme = t;
    }, 'Theme');
    return card({ title: 'Appearance', body: h('div.field', h('span.label', 'Theme'), seg) });
  }
  function sessionCard() {
    return card({ title: 'Session', body: h('div.stack', h('p.small.muted', 'You are signed in to this browser with a session cookie.'),
      btn('Sign out', { kind: 'danger', ico: 'logout', onClick: async () => { try { await request('POST', '/auth/logout', {}); } catch { /* */ } location.href = '/login'; } })) });
  }

  draw();
}
