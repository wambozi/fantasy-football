// ==UserScript==
// @name         Draft Copilot — draft history scrape
// @namespace    draft-copilot
// @version      0.2
// @description  Scrapes a finished FanDraft archived draft summary (ESPN recap as fallback) into normalised JSON, for building per-manager tendencies from past seasons. Read-only: it never posts a pick.
// @match        https://fandraft.app/*
// @match        https://*.fandraft.app/*
// @match        https://fantasy.espn.com/*
// @grant        GM_setClipboard
// ==/UserScript==

// HOW TO USE
// 1. Open an archived season's "Draft Summary" in FanDraft (All leagues -> the archived
//    year -> Draft Summary). Let the whole board render.
// 2. Click "Scrape draft" bottom-right, or paste this file into the DevTools console.
// 3. It downloads draft-<year>.json and copies the same JSON to the clipboard. Save it as
//    data/history/<year>.json.
//
// Read-only. It never contacts the server and never writes a pick.
//
// FanDraft renders each pick as:
//   <tr class="pick-row">
//     <td>1.4</td><td>Aaron *</td>
//     <td>Chase, Ja'Marr (CIN / WR)<span>Keeper</span><span>Traded Pick</span></td>
//   </tr>
// Note the identity column is the MANAGER (Aaron, Twizzler, "Jim & TCal"), not the team
// name — reconciling those against this year's draft-order.csv is a separate step.
// The trailing " *" on a manager marks a pick they did not originally own.

(() => {
  'use strict';

  const norm = (s) => (s || '').replace(/\s+/g, ' ').trim();
  const normPos = (p) => (p === 'DEF' || p === 'D/ST' || p === 'DST' ? 'DST' : p);

  // "Collins, Nico (HOU / WR)" -> {player, nfl_team, pos}. Suffixes stay attached to the
  // surname half, so "Chark Jr., DJ" comes back as "DJ Chark Jr.".
  function parsePlayer(text) {
    const m = norm(text).match(/^(.+?),\s*(.+?)\s*\(\s*([A-Za-z]{2,3})\s*\/\s*([A-Za-z/]+)\s*\)\s*$/);
    if (!m) return null;
    return { player: `${norm(m[2])} ${norm(m[1])}`, nfl_team: m[3].toUpperCase(), pos: normPos(m[4].toUpperCase()) };
  }

  // "1.4" -> {round: 1, pick: 4}
  function parseSlot(text) {
    const m = norm(text).match(/^(\d+)\.(\d+)$/);
    return m ? { round: +m[1], pick: +m[2] } : null;
  }

  function scrapeFanDraft() {
    const rows = document.querySelectorAll('tr.pick-row');
    const out = [];
    for (const row of rows) {
      const td = row.children;
      if (td.length < 3) continue;
      const slot = parseSlot(td[0].textContent);
      if (!slot) continue;
      // Strip the badge spans before reading the player, then read them for the flags.
      const cell = td[2].cloneNode(true);
      const badges = [...cell.querySelectorAll('span')].map((s) => norm(s.textContent).toUpperCase());
      cell.querySelectorAll('span').forEach((s) => s.remove());
      const p = parsePlayer(cell.textContent);
      if (!p) continue;
      out.push({
        round: slot.round,
        pick: slot.pick,
        slot: `${slot.round}.${slot.pick}`,
        manager: norm(td[1].textContent).replace(/\s*\*$/, ''),
        player: p.player,
        pos: p.pos,
        nfl_team: p.nfl_team,
        keeper: badges.includes('KEEPER') || undefined,
        traded_pick: badges.includes('TRADED PICK') || undefined,
      });
    }
    return out;
  }

  // ESPN recap fallback: "1  Derrick Henry Bal, RB  Lawson Country Lets Drive" laid out as
  // table cells, with "Round N" headings between blocks.
  function scrapeESPN() {
    const out = [];
    let round = 0;
    for (const line of document.body.innerText.split('\n')) {
      const t = norm(line);
      const rm = t.match(/^Round\s+(\d+)$/i);
      if (rm) { round = +rm[1]; continue; }
      const m = line.match(/^(\d{1,2})\t(.+?)\s+([A-Za-z]{2,3}|FA),\s*([A-Za-z/]+)\t(.+)$/);
      if (!m || !round) continue;
      out.push({
        round, pick: +m[1], slot: `${round}.${m[1]}`,
        manager: norm(m[5]), player: norm(m[2]),
        pos: normPos(m[4].toUpperCase()), nfl_team: m[3].toUpperCase(),
      });
    }
    return out;
  }

  function scrape() {
    const fd = scrapeFanDraft();
    return fd.length ? { picks: fd, how: 'fandraft tr.pick-row' } : { picks: scrapeESPN(), how: 'espn innerText' };
  }

  function go() {
    const { picks, how } = scrape();
    if (!picks.length) {
      alert('No picks found.\n\nIf the board is inside an iframe, switch the DevTools console frame selector off "top" and retry. Otherwise the markup has changed — send me one row\'s outerHTML.');
      return;
    }
    const managers = [...new Set(picks.map((p) => p.manager))].sort();
    const byRound = {};
    for (const p of picks) byRound[p.round] = (byRound[p.round] || 0) + 1;
    const rounds = Object.keys(byRound).map(Number).sort((a, b) => a - b);
    const ragged = rounds.filter((r) => byRound[r] !== managers.length);
    const year = (document.body.innerText.match(/ARCHIVED\s+(20\d\d)/i) || location.href.match(/(20\d\d)/) || [])[1] || '';

    console.log(`[history] ${picks.length} picks via ${how}; ${managers.length} managers, ${rounds.length} rounds`);
    console.log('[history] managers seen:\n  ' + managers.join('\n  '));
    console.log('[history] keepers:', picks.filter((p) => p.keeper).length, ' traded picks:', picks.filter((p) => p.traded_pick).length);
    if (ragged.length) console.warn('[history] rounds without one pick per manager:', ragged);

    const doc = { year: +year || null, source: location.hostname, scraped_at: new Date().toISOString(), managers, picks };
    const json = JSON.stringify(doc, null, 2);
    if (typeof GM_setClipboard === 'function') GM_setClipboard(json);
    const a = document.createElement('a');
    a.href = URL.createObjectURL(new Blob([json], { type: 'application/json' }));
    a.download = `draft-${year || 'unknown'}.json`;
    a.click();
    alert(`${picks.length} picks, ${managers.length} managers, ${rounds.length} rounds` +
      `\nkeepers ${picks.filter((p) => p.keeper).length}, traded picks ${picks.filter((p) => p.traded_pick).length}` +
      (ragged.length ? `\n\nWARNING: rounds ${ragged.join(', ')} are short — board may not be fully rendered.` : '') +
      `\n\nSaved draft-${year || 'unknown'}.json. Manager names are in the console.`);
  }

  const btn = document.createElement('button');
  btn.textContent = 'Scrape draft';
  btn.style.cssText = 'position:fixed;bottom:16px;right:16px;z-index:99999;padding:8px 12px;font:600 13px system-ui;background:#111;color:#fff;border:0;border-radius:4px;cursor:pointer';
  btn.onclick = go;
  const attach = () => document.body && document.body.appendChild(btn);
  if (document.readyState === 'complete' || document.readyState === 'interactive') attach();
  else addEventListener('load', attach);
})();
