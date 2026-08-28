// ==UserScript==
// @name         Draft Copilot — FanDraft recon
// @namespace    draft-copilot
// @version      0.1
// @description  Logs WebSocket frames, fetch/XHR JSON, and board DOM mutations from a FanDraft mock draft to the copilot server so the ingest parser can be written. Spec §9.1.
// @match        https://fandraft.app/*
// @match        https://*.fandraft.app/*
// @run-at       document-start
// @grant        GM_xmlhttpRequest
// @connect      localhost
// ==/UserScript==

// HOW TO USE
// 1. Install Tampermonkey, add this script, start `./server` on the laptop (port 8080).
// 2. Open a FanDraft mock draft. Let two or three picks happen.
// 3. Everything is appended to data/fandraft-frames.jsonl on the server and echoed in the
//    DevTools console with a [recon] prefix. Paste the interesting lines (a pick landing)
//    back into the chat; the ingest script's extractPick() gets written from them.
(() => {
  'use strict';
  const SERVER = 'http://localhost:8090';
  const post = (kind, payload) => {
    try {
      GM_xmlhttpRequest({
        method: 'POST', url: SERVER + '/api/fandraft/frame',
        headers: { 'Content-Type': 'application/json' },
        data: JSON.stringify({ raw: JSON.stringify({ kind, ...payload }), at: Date.now(), url: location.href }),
      });
    } catch (e) { console.warn('[recon] post failed', e); }
  };
  const clip = (s, n = 4000) => (typeof s === 'string' && s.length > n ? s.slice(0, n) + '…' : s);

  // 1. WebSocket frames (preferred path, §9.2).
  const Native = window.WebSocket;
  const Patched = function (...args) {
    const ws = new Native(...args);
    console.log('[recon] websocket opened', args[0]);
    post('ws-open', { target: String(args[0]) });
    ws.addEventListener('message', (e) => {
      const data = typeof e.data === 'string' ? e.data : '<binary ' + (e.data && e.data.byteLength) + ' bytes>';
      console.log('[recon] ws <-', clip(data, 800));
      post('ws', { target: String(args[0]), data: clip(data) });
    });
    const send = ws.send.bind(ws);
    ws.send = (d) => { console.log('[recon] ws ->', clip(String(d), 300)); post('ws-send', { data: clip(String(d)) }); return send(d); };
    return ws;
  };
  Patched.prototype = Native.prototype;
  Object.assign(Patched, { CONNECTING: 0, OPEN: 1, CLOSING: 2, CLOSED: 3 });
  window.WebSocket = Patched;

  // 2. fetch / XHR JSON responses (polled endpoint fallback).
  const nativeFetch = window.fetch;
  window.fetch = async (...args) => {
    const res = await nativeFetch(...args);
    try {
      const ct = res.headers.get('content-type') || '';
      if (ct.includes('json')) {
        const txt = await res.clone().text();
        const url = typeof args[0] === 'string' ? args[0] : args[0].url;
        console.log('[recon] fetch', url, clip(txt, 500));
        post('fetch', { url, data: clip(txt) });
      }
    } catch (e) { /* ignore */ }
    return res;
  };
  const open = XMLHttpRequest.prototype.open;
  XMLHttpRequest.prototype.open = function (method, url, ...rest) {
    this.addEventListener('load', () => {
      const ct = this.getResponseHeader('content-type') || '';
      if (ct.includes('json')) { console.log('[recon] xhr', url, clip(this.responseText, 500)); post('xhr', { url: String(url), data: clip(this.responseText) }); }
    });
    return open.call(this, method, url, ...rest);
  };

  // 3. DOM mutations (last resort, §9.3): summarise added nodes with a CSS-ish path.
  const path = (el) => {
    const parts = [];
    for (let e = el; e && e.nodeType === 1 && parts.length < 6; e = e.parentElement) {
      let s = e.tagName.toLowerCase();
      if (e.id) s += '#' + e.id;
      else if (e.className && typeof e.className === 'string') s += '.' + e.className.trim().split(/\s+/).slice(0, 3).join('.');
      parts.unshift(s);
    }
    return parts.join(' > ');
  };
  let batch = [];
  let timer = null;
  const flush = () => {
    if (!batch.length) return;
    const b = batch; batch = []; timer = null;
    console.log('[recon] dom', b.length, 'mutations', b.slice(0, 5));
    post('dom', { mutations: b.slice(0, 40) });
  };
  document.addEventListener('DOMContentLoaded', () => {
    new MutationObserver((muts) => {
      for (const m of muts) {
        for (const n of m.addedNodes) {
          if (n.nodeType !== 1) continue;
          const text = (n.textContent || '').trim().replace(/\s+/g, ' ').slice(0, 160);
          if (!text) continue;
          batch.push({ path: path(n), text });
        }
        if (m.type === 'characterData') {
          batch.push({ path: path(m.target.parentElement), text: (m.target.textContent || '').trim().slice(0, 160), changed: true });
        }
      }
      if (!timer) timer = setTimeout(flush, 500);
    }).observe(document.body, { childList: true, subtree: true, characterData: true });
    console.log('[recon] observing DOM');
  });
})();
