// noVNC wrapper. The server answers POST /api/v1/sandboxes/{id}/desktop?vnc=1 with
// {desktopUrl, wsUrl?, password?, provisioning?}. wsUrl is a websockify-style
// websocket carrying RFB. Without wsUrl we derive it from the token in desktopUrl.
import { h, icon, btn } from './dom.js';
import { api, errText } from './api.js';

// → {url, token}. The token travels as the Sec-WebSocket-Protocol entry
// "crowdvm-token.<t>", never in the URL (no tokens in logs or history).
export function wsURLFrom(id, r) {
  let u = r.wsUrl || r.websocketUrl;
  if (!u && r.desktopUrl && /^wss?:/.test(r.desktopUrl)) u = r.desktopUrl;
  const abs = new URL(u || `/api/v1/sandboxes/${id}/desktop/ws`, location.href);
  let token = r.token || abs.searchParams.get('token') || '';
  if (!token && r.desktopUrl) {
    const d = new URL(r.desktopUrl, location.href);
    token = d.searchParams.get('token') || new URLSearchParams(d.hash.slice(1)).get('token') || '';
  }
  abs.searchParams.delete('token');
  if (abs.protocol === 'http:') abs.protocol = 'ws:';
  if (abs.protocol === 'https:') abs.protocol = 'wss:';
  return { url: abs.href, token };
}

// Ask the server for a desktop session; polls while it is still provisioning.
export async function requestDesktop(id, signal, onWait) {
  const t = document.documentElement.dataset.theme;
  const theme = t === 'dark' || t === 'light' ? t : matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
  for (let i = 0; i < 40; i++) {
    const r = await api.post(`/sandboxes/${id}/desktop?vnc=1&theme=${theme}`, {}, { signal });
    if (!r.provisioning) return r;
    onWait?.(r.message || 'Starting the desktop session…');
    await new Promise((res) => setTimeout(res, 1500));
    if (signal?.aborted) throw new DOMException('aborted', 'AbortError');
  }
  throw new Error('The desktop did not start in time. Try again.');
}

// mount(screenEl, {url, token, password, viewOnly}) -> controller
export async function mountRFB(screen, { url, token, password, viewOnly = false, onStatus, background = '#0b0c0c' }) {
  const { default: RFB } = await import('../vendor/novnc/core/rfb.js');
  const rfb = new RFB(screen, url, { credentials: password ? { password } : undefined, wsProtocols: token ? ['binary', `crowdvm-token.${token}`] : ['binary'] });
  rfb.scaleViewport = true;
  rfb.resizeSession = true;
  rfb.viewOnly = viewOnly;
  rfb.focusOnClick = true;
  rfb.background = background;
  rfb.addEventListener('connect', () => onStatus?.('connected'));
  rfb.addEventListener('disconnect', (e) => onStatus?.(e.detail.clean ? 'disconnected' : 'failed'));
  rfb.addEventListener('credentialsrequired', () => onStatus?.('password'));
  rfb.addEventListener('securityfailure', (e) => onStatus?.('failed', e.detail.reason));
  return rfb;
}

// A complete viewer: overlay states + toolbar. Used by the tab and the full-screen page.
// token: a desktop token from a desktop URL. It is spent on the first connection;
// later reconnects ask the server for a fresh one when renew is true (signed in),
// so the viewer never depends on reusing a token (DESK-005: single-use links).
export function desktopViewer({ id, token, signal, full = false, title = '', overlay: chrome = true, theme = '', renew = true }) {
  const screen = h('div.screen', { 'aria-label': 'Remote desktop. Click to focus; keystrokes go to the sandbox.' });
  const overlay = h('div.overlay');
  const statusEl = h('span.small', { 'aria-live': 'polite' });
  let rfb = null, last = null, viewOnly = false;

  const setOverlay = (...kids) => { overlay.hidden = !kids.length; overlay.replaceChildren(...(kids.length ? [h('div.stack', ...kids)] : [])); };
  const viewBtn = btn('', { size: 'sm', ico: 'eye', title: 'View only (toggle)', onClick: () => { viewOnly = !viewOnly; if (rfb) rfb.viewOnly = viewOnly; viewBtn.setAttribute('aria-pressed', String(viewOnly)); } });
  viewBtn.setAttribute('aria-pressed', 'false');
  const cadBtn = btn('', { size: 'sm', ico: 'keyboard', title: 'Send Ctrl+Alt+Del', onClick: () => rfb?.sendCtrlAltDel() });
  const pasteBtn = btn('', { size: 'sm', ico: 'copy', title: 'Paste your clipboard into the sandbox', onClick: async () => {
    try { const t = await navigator.clipboard.readText(); rfb?.clipboardPasteFrom(t); } catch { /* permission denied */ }
  } });
  const fsBtn = btn('', { size: 'sm', ico: 'expand', title: 'Full screen', onClick: () => (document.fullscreenElement ? document.exitFullscreen() : frame.requestFullscreen?.()) });
  // A fresh URL for the new tab: the token in `last` was already spent on this connection.
  const openBtn = btn('', { size: 'sm', ico: 'link', title: 'Open in a new tab', onClick: async () => {
    const w = window.open('about:blank', '_blank');
    try {
      const r = await requestDesktop(id, signal);
      if (w) { w.opener = null; w.location = r.desktopUrl && !r.desktopUrl.startsWith('ws') ? r.desktopUrl : `/desktop/${id}`; }
    } catch (e) { w?.close(); statusEl.textContent = errText(e); }
  } });
  const reconnect = btn('Reconnect', { size: 'sm', ico: 'refresh', onClick: () => connect() });

  const tools = [statusEl, h('span.grow'), viewBtn, pasteBtn, cadBtn, full ? null : openBtn, fsBtn, reconnect];
  const frame = full
    ? h('div.desktop-full', chrome ? h('div.vbar', icon('monitor', 'sm'), h('b', title || id), ...tools) : null, h('div.screen', { style: { position: 'relative' } }, screen, overlay))
    : h('div.stack.tight', h('div.row', ...tools), h('div.desktop-frame', screen, overlay));
  if (full) Object.assign(screen.style, { position: 'absolute', inset: '0' });
  if (full && theme === 'light') frame.classList.add('light');
  let spent = false;

  async function connect() {
    rfb?.disconnect(); rfb = null;
    statusEl.textContent = 'connecting…';
    setOverlay(h('span.spinner.lg', { style: { margin: '0 auto' } }), h('div', 'Opening the desktop…'));
    try {
      const fromLink = token && !(spent && renew);
      if (fromLink) { last = { token }; spent = true; }
      else last = await requestDesktop(id, signal, (m) => setOverlay(h('span.spinner.lg', { style: { margin: '0 auto' } }), h('div', m)));
      const { url, token: tok } = wsURLFrom(id, last);
      rfb = await mountRFB(screen, { url, token: tok, password: last.password, viewOnly, background: theme === 'light' ? '#e9e9e5' : '#0b0c0c', onStatus: (s, why) => {
        statusEl.textContent = s;
        if (s === 'connected') { setOverlay(); rfb.focus(); }
        else if (s === 'password') {
          const pw = h('input.input', { type: 'password', placeholder: 'VNC password', 'aria-label': 'VNC password' });
          setOverlay(h('div', 'This desktop needs a password.'), pw, btn('Connect', { kind: 'primary', onClick: () => rfb.sendCredentials({ password: pw.value }) }));
          pw.focus();
        } else if (s === 'disconnected' || s === 'failed') {
          const linkOnly = fromLink && !renew;
          setOverlay(icon('monitor', 'lg'), h('div', s === 'failed' ? `Could not connect${why ? `: ${why}` : '.'}` : 'Disconnected.'),
            linkOnly ? h('div.small', 'Desktop links are valid for 10 minutes and stop working when the sandbox stops. Ask for a new one with ', h('code', `crowdvm desktop ${id}`), ' or open the sandbox in the dashboard.') : null,
            btn('Reconnect', { kind: 'primary', ico: 'refresh', onClick: connect }));
        }
      } });
    } catch (e) {
      if (e.name === 'AbortError') return;
      statusEl.textContent = 'error';
      setOverlay(icon('alert', 'lg'), h('div', errText(e)), btn('Try again', { kind: 'primary', ico: 'refresh', onClick: connect }));
    }
  }
  signal?.addEventListener('abort', () => rfb?.disconnect());
  return { el: frame, connect, get rfb() { return rfb; } };
}
