// Approve a CLI device login: /device?code=XXXX-XXXX
import { h, icon } from '../dom.js';
import { auth, errText } from '../api.js';
import { brand } from '../ui.js';

export default async function (ctx) {
  ctx.title('Approve device');
  const me = ctx.session.me || {};
  const code = (ctx.query.get('code') || '').toUpperCase();
  const input = h('input.input.mono', { id: 'code', value: code, autocomplete: 'off', autocapitalize: 'characters', spellcheck: 'false', placeholder: 'ABCD-EFGH', style: { textAlign: 'center', fontSize: '18px', letterSpacing: '.12em', height: '44px' } });
  const msg = h('div', { role: 'status', 'aria-live': 'polite' });
  const approve = h('button.btn.primary', { type: 'submit' }, 'Approve sign-in');
  const body = h('div.stack');
  const form = h('form.stack', { novalidate: true },
    h('div.field', h('label', { for: 'code' }, 'Code shown in your terminal'), input),
    h('div.notice', icon('info'), h('div', 'Approve only if you just ran ', h('code', 'crowdvm login'), '. The CLI gets an API key that acts as ', h('b', me.login || me.username || 'you'), '.')),
    msg, approve,
    h('a.btn.ghost', { href: '/sandboxes' }, 'Cancel'));
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    const c = input.value.trim().toUpperCase();
    if (!c) { input.focus(); return; }
    approve.disabled = true; msg.replaceChildren();
    try {
      await auth.approveDevice(c);
      body.replaceChildren(h('div.empty', h('div.ico', { style: { background: 'var(--ok-soft)', color: 'var(--ok-text)' } }, icon('check', 'lg')),
        h('h3', 'Device approved'), h('p', 'Go back to your terminal. You can close this tab.'),
        h('a.btn', { href: '/sandboxes' }, 'Open dashboard')));
    } catch (e2) {
      approve.disabled = false;
      msg.replaceChildren(h('div.notice.err', icon('alert'), h('div', e2.status === 404 ? 'That code is unknown or expired. Run crowdvm login again.' : errText(e2))));
    }
  });
  body.append(h('h1', 'Sign in a device'), h('p.sub', 'The CrowdVM CLI is asking to sign in.'), form);
  ctx.root.append(h('div.auth', h('div.auth-card', h('div.card', h('div.card-body', brand('/sandboxes'), body)))));
  (code ? approve : input).focus();
}
