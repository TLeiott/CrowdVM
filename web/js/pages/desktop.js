// Full-screen desktop: /desktop/{id}[?theme=light|dark][&view=browser][&overlay=0][#token=…].
// Works without a session when the URL carries a desktop token (the URL the API
// hands out); otherwise uses the cookie. The token is read once and removed from
// the address bar so it does not stay in history or get shared by copy/paste.
import { desktopViewer } from '../vnc.js';

export default async function (ctx) {
  const id = ctx.params.id;
  const hash = new URLSearchParams(location.hash.slice(1));
  const token = hash.get('token') || ctx.query.get('token') || '';
  if (hash.has('token') || ctx.query.has('token')) {
    const q = new URLSearchParams(location.search); q.delete('token');
    history.replaceState(history.state, '', location.pathname + (q.size ? `?${q}` : ''));
  }
  const theme = ctx.query.get('theme');
  if (theme === 'light' || theme === 'dark') document.documentElement.dataset.theme = theme;
  const browser = ctx.query.get('view') === 'browser';
  ctx.title(`${browser ? 'Browser' : 'Desktop'} ${id}`);
  if (!token && !ctx.session.me) return ctx.nav(`/login?next=${encodeURIComponent(location.pathname + location.search)}`, { replace: true });
  const v = desktopViewer({ id, token: token || null, signal: ctx.signal, full: true, title: browser ? `${id} · browser` : id,
    overlay: ctx.query.get('overlay') !== '0', theme: theme || '', renew: !!ctx.session.me && !browser });
  ctx.root.append(v.el);
  v.connect();
}
