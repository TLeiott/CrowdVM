// Tiny DOM + formatting helpers. h('div.card#x', {attrs}, ...children)
import { icon } from './icons.js';

export function h(sel, attrs, ...kids) {
  if (attrs == null || typeof attrs !== 'object' || attrs instanceof Node || Array.isArray(attrs)) {
    if (attrs !== undefined) kids.unshift(attrs);
    attrs = {};
  }
  const m = /^([a-z0-9-]*)((?:[.#][\w-]+)*)$/i.exec(sel);
  const el = document.createElement((m && m[1]) || 'div');
  if (m && m[2]) for (const p of m[2].match(/[.#][\w-]+/g)) p[0] === '.' ? el.classList.add(p.slice(1)) : (el.id = p.slice(1));
  for (const [k, v] of Object.entries(attrs)) {
    if (v == null || v === false) continue;
    if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2).toLowerCase(), v);
    else if (k === 'class') el.className += (el.className ? ' ' : '') + v;
    else if (k === 'style' && typeof v === 'object') Object.assign(el.style, v);
    else if (k === 'dataset') Object.assign(el.dataset, v);
    else if (k in el && !k.includes('-') && k !== 'list' && k !== 'form' && typeof v !== 'string') el[k] = v;
    else if (k === 'value' || k === 'checked' || k === 'selected') el[k] = v;
    else el.setAttribute(k, v === true ? '' : v);
  }
  append(el, kids);
  return el;
}

function append(el, kids) {
  for (const k of kids) {
    if (k == null || k === false || k === true) continue;
    if (Array.isArray(k)) append(el, k);
    else el.append(k instanceof Node ? k : String(k));
  }
}

export const $ = (s, r = document) => r.querySelector(s);
export const $$ = (s, r = document) => [...r.querySelectorAll(s)];
export { icon };

export function btn(label, opts = {}) {
  const { kind = '', ico, onClick, title, type = 'button', size, disabled, attrs = {} } = opts;
  const cls = ['btn', kind, size, !label && ico ? 'icon' : ''].filter(Boolean).join(' ');
  return h('button', { type, class: cls, onclick: onClick, title, 'aria-label': !label ? title : null, disabled, ...attrs },
    ico ? icon(ico) : null, label || null);
}

// ---------------- formatting ----------------
const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: 'auto', style: 'short' });
export function ago(ts) {
  if (!ts) return '—';
  const d = new Date(typeof ts === 'number' && ts < 1e12 ? ts * 1000 : ts);
  if (isNaN(d)) return '—';
  const s = (d - Date.now()) / 1000, a = Math.abs(s);
  if (a < 45) return s < 0 ? 'just now' : 'in a few seconds';
  for (const [u, n] of [['minute', 60], ['hour', 3600], ['day', 86400], ['month', 2592000], ['year', 31536000]].reverse())
    if (a >= n * 0.9) return rtf.format(Math.round(s / n), u);
  return rtf.format(Math.round(s / 60), 'minute');
}
export function dateTime(ts) {
  if (!ts) return '—';
  const d = new Date(ts);
  return isNaN(d) ? '—' : d.toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' });
}
export function time(ts, attrs = {}) {
  return h('time', { datetime: ts || '', title: dateTime(ts), ...attrs }, ago(ts));
}
export function bytes(n, dp = 1) {
  if (n == null || isNaN(n)) return '—';
  const u = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
  let i = 0; n = Number(n);
  while (Math.abs(n) >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return `${n.toFixed(i && n < 100 ? dp : 0)} ${u[i]}`;
}
export const mib = (m) => (m == null ? '—' : m === 0 ? '0' : m >= 1024 ? `${+(m / 1024).toFixed(m % 1024 ? 1 : 0)} GiB` : `${m} MiB`);
export function duration(sec) {
  if (sec == null) return 'never';
  sec = Math.round(sec);
  if (sec < 60) return `${sec}s`;
  const d = Math.floor(sec / 86400), hh = Math.floor((sec % 86400) / 3600), m = Math.floor((sec % 3600) / 60);
  return [d && `${d}d`, hh && `${hh}h`, m && `${m}m`].filter(Boolean).slice(0, 2).join(' ') || '0m';
}
export const num = (n, dp = 0) => (n == null || isNaN(n) ? '—' : Number(n).toLocaleString(undefined, { maximumFractionDigits: dp, minimumFractionDigits: 0 }));
export const plural = (n, w, ws = w + 's') => `${num(n)} ${n === 1 ? w : ws}`;

// Parse "1h", "30m", "2d", "3600" or "never" into seconds (null = never).
export function parseTTL(s) {
  s = String(s || '').trim().toLowerCase();
  if (!s || s === 'never' || s === 'none' || s === '0') return null;
  const m = /^(\d+(?:\.\d+)?)\s*([smhdw]?)$/.exec(s);
  if (!m) return undefined;
  return Math.round(parseFloat(m[1]) * { '': 1, s: 1, m: 60, h: 3600, d: 86400, w: 604800 }[m[2]]);
}

export function initials(name = '?') {
  const p = String(name).trim().split(/[\s._-]+/).filter(Boolean);
  return ((p[0]?.[0] || '?') + (p[1]?.[0] || '')).toUpperCase();
}

export async function copy(text) {
  try { await navigator.clipboard.writeText(text); return true; }
  catch {
    const ta = h('textarea', { style: { position: 'fixed', opacity: 0 } }, text);
    document.body.append(ta); ta.select();
    const ok = document.execCommand('copy'); ta.remove(); return ok;
  }
}

// Run fn now and every ms while the page is visible, until signal aborts.
export function every(ms, fn, signal) {
  let t, stopped = false;
  const tick = async () => {
    if (stopped) return;
    if (!document.hidden) { try { await fn(); } catch { /* shown by caller */ } }
    if (!stopped) t = setTimeout(tick, ms);
  };
  const vis = () => { if (!document.hidden && !stopped) { clearTimeout(t); tick(); } };
  document.addEventListener('visibilitychange', vis);
  signal?.addEventListener('abort', () => { stopped = true; clearTimeout(t); document.removeEventListener('visibilitychange', vis); });
  t = setTimeout(tick, ms);
}

// Replace el's children only when the rendered data changed (keeps focus/scroll stable while polling).
export function patch(el, data, render) {
  const key = JSON.stringify(data);
  if (el._key === key) return;
  el._key = key;
  el.replaceChildren(...[render(data)].flat().filter(Boolean));
}
