// ==UserScript==
// @name         Draft Copilot — FanDraft ingest
// @namespace    draft-copilot
// @version      0.1
// @description  Forwards picks from the FanDraft live board to the copilot server. WebSocket path first, DOM observer fallback. Spec §9.2–9.3.
// @match        https://fandraft.app/*
// @match        https://*.fandraft.app/*
// @run-at       document-start
// @grant        GM_xmlhttpRequest
// @connect      localhost
// ==/UserScript==

// CONFIG — the only part that changes after §9.1 recon. Everything below it is generic.
const CONFIG = {
  server: 'http://localhost:8080',

  // WebSocket path. Return {overall|round+pick, player, team} or null for a frame that is
  // not a pick (chat, clock ticks). `msg` is the parsed JSON when the frame was JSON,
  // else the raw string. Filled in once recon shows the wire shape.
  extractPickFromFrame(msg) {
    if (!msg || typeof msg !== 'object') return null;
    // EXAMPLE shape — replace with the real one:
    // if (msg.type === 'pick' && msg.player) return { overall: msg.pickNumber, player: msg.player.name, team: msg.team?.name };
    return null;
  },

  // DOM fallback. A selector for the elements that represent filled board cells, and a
  // function that turns one such element into {overall|round+pick, player, team} or null.
  dom: {
    enabled: true,
    cellSelector: '[class*="pick"], [class*="cell"], [data-pick]',
    extract(el) {
      const t = (el.textContent || '').replace(/\s+/g, ' ').trim();
      // EXAMPLE: "3.05 Quinshon Judkins RB CLE" — replace with the real cell layout.
      const m = t.match(/^(\d+)\.(\d+)\s+(.+?)\s+(QB|RB|WR|TE|K|D\/?ST|DEF)\b/i);
      if (!m) return null;
      return { round: +m[1], pick: +m[2], player: m[3] };
    },
  },
};

(() => {
  'use strict';
  const sent = new Set(); // de-dupe by slot+player within the page; the server de-dupes too
  const send = (pick, source) => {
    const key = `${pick.overall || ''}|${pick.round || ''}.${pick.pick || ''}|${pick.player}`;
    if (sent.has(key)) return;
    sent.add(key);
    GM_xmlhttpRequest({
      method: 'POST', url: CONFIG.server + '/api/fandraft/pick',
      headers: { 'Content-Type': 'application/json' },
      data: JSON.stringify({ ...pick, source }),
      onload: (r) => console.log('[ingest]', source, pick.player, r.responseText.slice(0, 120)),
      onerror: () => { sent.delete(key); console.warn('[ingest] server unreachable'); },
    });
  };

  // WebSocket interception (§9.2).
  const Native = window.WebSocket;
  const Patched = function (...args) {
    const ws = new Native(...args);
    ws.addEventListener('message', (e) => {
      if (typeof e.data !== 'string') return;
      let msg = e.data;
      try { msg = JSON.parse(e.data); } catch (_) { /* raw */ }
      try {
        const p = CONFIG.extractPickFromFrame(msg);
        if (p && p.player) send(p, 'ws');
      } catch (err) { console.warn('[ingest] extract failed', err); }
    });
    return ws;
  };
  Patched.prototype = Native.prototype;
  Object.assign(Patched, { CONNECTING: 0, OPEN: 1, CLOSING: 2, CLOSED: 3 });
  window.WebSocket = Patched;

  // DOM observer (§9.3): scan on load and on every mutation, throttled.
  if (CONFIG.dom.enabled) {
    let timer = null;
    const scan = () => {
      timer = null;
      document.querySelectorAll(CONFIG.dom.cellSelector).forEach((el) => {
        try {
          const p = CONFIG.dom.extract(el);
          if (p && p.player) send(p, 'dom');
        } catch (_) { /* ignore */ }
      });
    };
    document.addEventListener('DOMContentLoaded', () => {
      scan();
      new MutationObserver(() => { if (!timer) timer = setTimeout(scan, 400); })
        .observe(document.body, { childList: true, subtree: true, characterData: true });
    });
  }

  // Heartbeat so the copilot's freshness indicator shows the script is alive even
  // between picks (an empty frame is stored, never applied).
  setInterval(() => {
    GM_xmlhttpRequest({ method: 'POST', url: CONFIG.server + '/api/fandraft/frame', headers: { 'Content-Type': 'application/json' },
      data: JSON.stringify({ raw: '{"kind":"heartbeat"}', at: Date.now(), url: location.href }) });
  }, 20000);
})();
