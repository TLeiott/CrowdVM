// Accept an organization invite link: /invite?code=cvi_…
import { h, icon } from '../dom.js';
import { api, errText } from '../api.js';
import { brand } from '../ui.js';

export default async function (ctx) {
  ctx.title('Join organization');
  const code = ctx.query.get('code') || '';
  const body = h('div.stack');
  const join = h('button.btn.primary', { type: 'button' }, 'Join organization');
  const msg = h('div', { role: 'status', 'aria-live': 'polite' });
  join.addEventListener('click', async () => {
    join.disabled = true; msg.replaceChildren();
    try {
      const r = await api.post('/org-invites/accept', { code });
      await ctx.refreshShell?.();
      body.replaceChildren(h('div.empty', h('div.ico', { style: { background: 'var(--ok-soft)', color: 'var(--ok-text)' } }, icon('check', 'lg')),
        h('h3', `You joined ${r.org?.name || 'the organization'}`), h('p', 'Pick it in the sidebar’s Viewing list to see its sandboxes and bill the ones you create there.'),
        h('a.btn.primary', { href: '/org' }, 'Open organization')));
    } catch (e) {
      join.disabled = false;
      msg.replaceChildren(h('div.notice.err', icon('alert'), h('div', e.status === 404 ? 'This invite is wrong, used, revoked or expired. Ask for a new link.' : errText(e))));
    }
  });
  const me = ctx.session.me || {};
  body.append(h('h1', 'Join an organization'),
    h('p.sub', 'You were invited to share sandboxes and a credit wallet with a team.'),
    code ? h('div.stack', h('div.notice', icon('info'), h('div', 'You join as ', h('b', me.login || me.username || 'you'), '. Your personal sandboxes and snapshots stay yours.')), msg, join,
      h('a.btn.ghost', { href: '/sandboxes' }, 'Not now'))
      : h('div.notice.err', icon('alert'), h('div', 'The link has no invite code.')));
  ctx.root.append(h('div.auth', h('div.auth-card', h('div.card', h('div.card-body', brand('/sandboxes'), body)))));
  join.focus();
}
