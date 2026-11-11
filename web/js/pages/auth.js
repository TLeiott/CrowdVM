// Sign in / sign up.
import { h, icon } from '../dom.js';
import { auth, errText } from '../api.js';
import { brand } from '../ui.js';

export default async function (ctx) {
  const signup = ctx.path === '/signup';
  ctx.title(signup ? 'Create account' : 'Sign in');
  const next = ctx.query.get('next') || '/sandboxes';
  const err = h('div.notice.err', { role: 'alert', hidden: true });
  const user = h('input.input', { name: 'username', autocomplete: 'username', required: true, autocapitalize: 'none', spellcheck: 'false', minlength: 2 });
  const pass = h('input.input', { name: 'password', type: 'password', autocomplete: signup ? 'new-password' : 'current-password', required: true, minlength: signup ? 8 : 1 });
  const submit = h('button.btn.primary', { type: 'submit' }, signup ? 'Create account' : 'Sign in');
  const form = h('form.stack', { novalidate: true },
    err,
    h('div.field', h('label', { for: 'u' }, 'Username'), Object.assign(user, { id: 'u' })),
    h('div.field', h('label', { for: 'p' }, 'Password'), Object.assign(pass, { id: 'p' }),
      signup ? h('div.hint', 'At least 8 characters.') : null),
    submit);
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    err.hidden = true;
    if (!user.value.trim() || !pass.value) { err.replaceChildren(icon('alert'), h('div', 'Enter a username and a password.')); err.hidden = false; return; }
    if (signup && pass.value.length < 8) { err.replaceChildren(icon('alert'), h('div', 'Use at least 8 characters for the password.')); err.hidden = false; pass.focus(); return; }
    submit.disabled = true; submit.prepend(h('span.spinner'));
    try {
      const r = await (signup ? auth.signup : auth.login)({ username: user.value.trim(), password: pass.value });
      ctx.session.me = r.user || r;
      // full navigation: rebuilds the shell with the new session
      location.replace(next.startsWith('/') && !next.startsWith('//') ? next : '/sandboxes');
    } catch (e2) {
      err.replaceChildren(icon('alert'), h('div', e2.status === 401 ? 'Wrong username or password.' : errText(e2)));
      err.hidden = false;
      submit.disabled = false; submit.querySelector('.spinner')?.remove();
      pass.select();
    }
  });
  const q = ctx.query.toString();
  ctx.root.append(h('div.auth', h('div.auth-card',
    h('div.card', h('div.card-body',
      brand('/'),
      h('h1', signup ? 'Create your account' : 'Sign in to CrowdVM'),
      h('p.sub', signup ? 'Run persistent Ubuntu sandboxes on machines your crowd shares.' : 'Persistent Ubuntu sandboxes on your crowd’s machines.'),
      form,
      h('p.alt', signup ? ['Already have an account? ', h('a', { href: `/login${q ? '?' + q : ''}` }, 'Sign in')]
        : ['New here? ', h('a', { href: `/signup${q ? '?' + q : ''}` }, 'Create an account')]))),
    h('p.alt.faint', { style: { marginTop: '16px' } }, 'Self-hosted · LAN & Tailscale'))));
  user.focus();
}
