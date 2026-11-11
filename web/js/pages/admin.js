// Server admin: users (create, admin, suspend, password, credit grants), signup invite codes, retention.
import { h, btn, time, num } from '../dom.js';
import { api, errText } from '../api.js';
import { pageHead, card, table, loading, errorBox, modal, field, toast, toastErr, confirmDialog, secretDialog, menuButton, checkbox, empty, segmented } from '../ui.js';

export default async function (ctx) {
  ctx.title('Admin');
  if (!ctx.session.me?.isAdmin) {
    ctx.root.append(h('div.card', empty({ ico: 'shield', title: 'Admins only', text: 'This page is for the server’s administrators.' })));
    return;
  }
  const users = h('div', loading()), codes = h('div', loading()), ret = h('div', loading());
  ctx.root.append(
    pageHead({ title: 'Admin', lede: 'Accounts on this server, signup invite codes and the data retention job.',
      actions: btn('New user', { kind: 'primary', ico: 'plus', onClick: newUser, attrs: { 'data-create': '' } }) }),
    h('div.stack.loose',
      card({ title: 'Users', flush: true, body: users }),
      h('div.grid.side',
        card({ title: 'Invite codes', sub: 'Signup is invite-only unless the server runs with --open-signup.', flush: true, actions: btn('New code', { size: 'sm', ico: 'plus', onClick: newCode }), body: codes }),
        card({ title: 'Data retention', body: ret }))));

  const me = ctx.session.me;
  const patchUser = (u, body, msg) => api.patch(`/admin/users/${encodeURIComponent(u.id)}`, body).then(() => { toast(msg); loadUsers(); }, toastErr);

  async function loadUsers() {
    try {
      const r = await api.get('/admin/users', undefined, { signal: ctx.signal });
      users.replaceChildren(table({
        rows: r.users || [],
        rowLabel: (u) => u.login,
        cols: [
          { label: 'User', render: (u) => h('div.stack.tight', { style: { gap: 0 } }, h('b', u.displayName || u.login), h('span.faint.small', `@${u.login}${u.email ? ` · ${u.email}` : ''}`)) },
          { label: 'Status', render: (u) => h('div.row.wrap', { style: { gap: '4px' } },
            u.isAdmin ? h('span.chip.accent.plain', 'admin') : null,
            u.suspended || u.disabled ? h('span.chip.err.plain', 'suspended') : u.closed ? h('span.chip.warn.plain', 'closed') : h('span.chip.ok.plain', 'active'),
            u.serviceAccount ? h('span.chip.plain', 'service') : null) },
          { label: 'Credits', cls: 'num', render: (u) => num(u.credits, 2) },
          { label: 'Running', cls: 'num hide-xs', render: (u) => String(u.activeSandboxes ?? 0) },
          { label: 'Joined', cls: 'hide-md', render: (u) => h('span.muted', time(u.createdAt)) },
          { label: h('span.sr-only', 'Actions'), cls: 'act', render: (u) => menuButton(() => [
            { label: 'Grant credits…', ico: 'coins', onClick: () => grant(u) },
            { label: 'Limits…', ico: 'shield', onClick: () => policy(u.id, u.login) },
            { label: 'Set password…', ico: 'lock', onClick: () => setPassword(u) },
            u.id !== me.id ? { label: u.isAdmin ? 'Remove admin rights' : 'Make admin', ico: 'shield', onClick: () => patchUser(u, { admin: !u.isAdmin }, u.isAdmin ? `${u.login} is no longer an admin` : `${u.login} is now an admin`) } : null,
            u.id !== me.id ? '-' : null,
            u.id !== me.id ? (u.suspended || u.disabled
              ? { label: 'Unsuspend', ico: 'play', onClick: () => patchUser(u, { disabled: false }, `${u.login} can sign in again`) }
              : { label: 'Suspend…', ico: 'pause', danger: true, onClick: async () => {
                if (await confirmDialog({ title: `Suspend ${u.login}?`, desc: 'They cannot sign in or use API keys until you unsuspend them.', confirm: 'Suspend', danger: true })) patchUser(u, { disabled: true }, `${u.login} suspended`);
              } }) : null,
          ], { title: `Actions for ${u.login}` }) },
        ],
      }));
    } catch (e) { if (e.name !== 'AbortError') users.replaceChildren(errorBox(e, loadUsers)); }
  }

  async function loadCodes() {
    try {
      const r = await api.get('/admin/invite-codes', undefined, { signal: ctx.signal });
      codes.replaceChildren(table({
        rows: r.inviteCodes || [],
        emptyEl: h('div.card-body.faint', 'No invite codes.'),
        cols: [
          { label: 'Code', render: (c) => h('div.stack.tight', { style: { gap: 0 } }, h('code', `${c.prefix}…`), c.note ? h('span.faint.small', c.note) : null) },
          { label: 'Used', cls: 'num', render: (c) => `${c.uses}${c.maxUses ? ` / ${c.maxUses}` : ''}` },
          { label: 'State', cls: 'hide-xs', render: (c) => c.revokedAt ? h('span.chip.plain', 'revoked') : c.expiresAt && new Date(c.expiresAt) < Date.now() ? h('span.chip.err.plain', 'expired') : c.maxUses && c.uses >= c.maxUses ? h('span.chip.plain', 'used up') : h('span.chip.ok.plain', 'active') },
          { label: h('span.sr-only', 'Actions'), cls: 'act', render: (c) => c.revokedAt ? null : btn('', { size: 'sm', kind: 'ghost', ico: 'trash', title: `Revoke ${c.prefix}`, onClick: () =>
            api.del(`/admin/invite-codes/${encodeURIComponent(c.id)}`).then(() => { toast('Code revoked'); loadCodes(); }, toastErr) }) },
        ],
      }));
    } catch (e) { if (e.name !== 'AbortError') codes.replaceChildren(errorBox(e, loadCodes)); }
  }

  async function loadRetention() {
    try {
      const { retention: r } = await api.get('/admin/retention', undefined, { signal: ctx.signal });
      const days = h('input.input', { type: 'number', min: 1, value: r.stoppedDays, style: { maxWidth: '120px' } });
      let mode = r.mode;
      const rep = r.lastReport || {};
      ret.replaceChildren(h('div.stack',
        h('div.field', h('span.label', 'Mode'), segmented([['report', 'Report only'], ['enforce', 'Enforce']], mode, (v) => { mode = v; }, 'Retention mode'),
          h('div.hint', 'Report only lists what would expire. Enforce deletes the snapshot data of sandboxes stopped longer than the limit.')),
        field('Keep stopped sandboxes’ data for (days)', days),
        h('p.small.muted', r.lastRunAt ? ['Last run ', time(r.lastRunAt), `: ${num(rep.expiredSandboxes ?? 0)} sandboxes ${rep.mode === 'enforce' ? 'expired' : 'would expire'}, ${num(rep.olderSnapshots ?? 0)} older snapshots.`] : 'Not run yet.'),
        h('div.row.end', btn('Save', { size: 'sm', kind: 'primary', onClick: async () => {
          if (mode === 'enforce' && r.mode !== 'enforce' && !(await confirmDialog({ title: 'Enforce retention?', danger: true, confirm: 'Enforce', desc: `Snapshot data of sandboxes stopped more than ${days.value} days ago is deleted at the next run. They cannot be resumed afterwards.` }))) return;
          try { await api.put('/admin/retention', { mode, stoppedDays: Number(days.value) }); toast('Retention saved'); loadRetention(); } catch (e) { toastErr(e); }
        } }))));
    } catch (e) { if (e.name !== 'AbortError') ret.replaceChildren(h('p.faint', errText(e))); }
  }

  function newUser() {
    const login = h('input.input.mono', { autocomplete: 'off', spellcheck: 'false' }), pw = h('input.input', { type: 'password', autocomplete: 'new-password' });
    const email = h('input.input', { type: 'email' }), credits = h('input.input', { type: 'number', step: 'any', placeholder: 'server default' });
    const admin = checkbox('admin', 'Administrator', 'Can manage users, nodes and server settings.');
    modal({ title: 'New user', submit: true,
      body: h('div.stack', h('div.fields', field('Username', login), field('Password', pw, 'At least 8 characters.'), field('Email', email, 'Optional.'), field('Starting credits', credits)), admin),
      actions: [{ label: 'Cancel', onClick: () => true }, { label: 'Create', kind: 'primary', submit: true, onClick: async () => {
        await api.post('/admin/users', { username: login.value.trim(), password: pw.value, email: email.value.trim() || undefined,
          admin: admin.querySelector('input').checked, credits: credits.value === '' ? undefined : Number(credits.value) });
        toast(`Created ${login.value.trim()}`); loadUsers();
      } }] });
  }
  function grant(u) {
    const amount = h('input.input', { type: 'number', step: 'any', placeholder: '10' }), reason = h('input.input', { placeholder: 'Optional, shown in their ledger' });
    modal({ title: `Grant credits to ${u.login}`, submit: true, desc: `Balance now: ${num(u.credits, 2)} credits. A negative amount takes credits away.`,
      body: h('div.stack', field('Credits', amount), field('Reason', reason)),
      actions: [{ label: 'Cancel', onClick: () => true }, { label: 'Grant', kind: 'primary', submit: true, onClick: async () => {
        const n = Number(amount.value);
        if (!n) throw new Error('Enter an amount other than 0.');
        await api.post(`/admin/users/${encodeURIComponent(u.id)}/credits`, { amount: n, reason: reason.value.trim() || undefined });
        toast(`${n > 0 ? 'Granted' : 'Removed'} ${num(Math.abs(n), 2)} credits`); loadUsers(); ctx.refreshShell();
      } }] });
  }
  // Wallet policy: credit quota and caps, start limits, max TTL, sizes, snapshot allowance, low-credit alert.
  const POLICY = [
    ['monthlyCredits', 'Monthly credit quota', 'Credits added each month.', 'float'],
    ['creditCapMonthly', 'Monthly credit cap', 'Sandboxes stop when the month’s spend reaches this.', 'float'],
    ['lowCreditAlert', 'Low-credit alert at', 'Notice and mail when the balance drops below this.', 'float'],
    ['maxActive', 'Sandboxes at once', '', 'int'],
    ['startsPerMinute', 'Starts per minute', '', 'int'],
    ['startsPerHour', 'Starts per hour', '', 'int'],
    ['startsPerDay', 'Starts per day', '', 'int'],
    ['maxTtlSeconds', 'Longest auto-stop (hours)', 'Sandboxes cannot run longer without a restart.', 'hours'],
    ['savedSnapshotAllowance', 'Saved snapshots', '', 'int'],
  ];
  async function policy(wallet, label) {
    const box = h('div', loading());
    const inputs = {};
    modal({ title: `Limits for ${label}`, wide: true, desc: 'Empty fields use the server default.', body: box,
      actions: [{ label: 'Cancel', onClick: () => true }, { label: 'Save', kind: 'primary', onClick: async () => {
        const body = {};
        for (const [k, , , t] of POLICY) {
          const v = inputs[k].value.trim();
          body[k] = v === '' ? null : t === 'hours' ? Math.round(Number(v) * 3600) : t === 'int' ? parseInt(v, 10) : Number(v);
          if (body[k] != null && !Number.isFinite(body[k])) throw new Error(`Not a number: ${v}`);
        }
        const sizes = [...box.querySelectorAll('input[name=size]:checked')].map((i) => i.value);
        body.allowedSizes = sizes.length === 4 ? null : sizes;
        await api.put(`/admin/policies/${encodeURIComponent(wallet)}`, body); toast('Limits saved');
      } }] });
    try {
      const { policy: p } = await api.get(`/admin/policies/${encodeURIComponent(wallet)}`);
      const eff = p.effective || {};
      box.replaceChildren(h('div.stack',
        h('div.fields', POLICY.map(([k, l, hint, t]) => {
          const v = p[k] == null ? '' : t === 'hours' ? p[k] / 3600 : p[k];
          inputs[k] = h('input.input', { type: 'number', step: t === 'int' ? '1' : 'any', min: 0, value: v, placeholder: eff[k] != null ? `default ${eff[k]}` : 'no limit' });
          return field(l, inputs[k], hint || null);
        })),
        h('div.field', h('span.label', 'Allowed sizes'), h('div.row.wrap', ['small', 'default', 'large', 'xlarge'].map((z) =>
          h('label.check', h('input', { type: 'checkbox', name: 'size', value: z, checked: !p.allowedSizes || p.allowedSizes.includes(z) }), h('span', { style: { textTransform: 'capitalize' } }, z)))))));
    } catch (e) { box.replaceChildren(errorBox(e)); }
  }

  function setPassword(u) {
    const pw = h('input.input', { type: 'password', autocomplete: 'new-password' });
    modal({ title: `Set a password for ${u.login}`, submit: true, body: field('New password', pw, 'At least 8 characters. Tell them over a safe channel.'),
      actions: [{ label: 'Cancel', onClick: () => true }, { label: 'Set password', kind: 'primary', submit: true, onClick: async () => {
        if (pw.value.length < 8) throw new Error('Use at least 8 characters.');
        await api.patch(`/admin/users/${encodeURIComponent(u.id)}`, { password: pw.value }); toast('Password set');
      } }] });
  }
  function newCode() {
    const note = h('input.input', { placeholder: 'e.g. for the lab team' }), uses = h('input.input', { type: 'number', min: 1, placeholder: '1' });
    let ttl = '604800';
    modal({ title: 'New invite code', submit: true,
      body: h('div.stack', field('Note', note), field('Max signups', uses),
        h('div.field', h('span.label', 'Valid for'), segmented([['86400', '1 day'], ['604800', '7 days'], ['2592000', '30 days']], ttl, (v) => { ttl = v; }, 'Valid for'))),
      actions: [{ label: 'Cancel', onClick: () => true }, { label: 'Create', kind: 'primary', submit: true, onClick: async () => {
        const r = await api.post('/admin/invite-codes', { note: note.value.trim(), maxUses: Number(uses.value) || 1, ttlSeconds: Number(ttl) });
        secretDialog({ title: 'Invite code created', secret: r.code, extra: r.signupUrl ? h('div.stack.tight', h('span.small.muted', 'Or send the signup link:'), h('code.small', { style: { overflowWrap: 'anywhere' } }, r.signupUrl)) : null });
        loadCodes();
      } }] });
  }

  await Promise.all([loadUsers(), loadCodes(), loadRetention()]);
}
