// Draft Copilot UI. One screen, keyboard-first, no framework, no build.
//
// Data flow: GET /api/state on load, then SSE /api/stream pushes the full payload on
// every version bump. If the stream drops we poll every 2s until it comes back.
(() => {
  "use strict";
  const $ = (id) => document.getElementById(id);
  const el = {
    app: $("app"), status: $("status"), pick: $("st-pick"), turn: $("st-turn"), gate: $("st-gate"),
    auto: $("st-auto"), conflict: $("conflict"), conflictText: $("conflict-text"), conflictOk: $("conflict-ok"),
    recs: $("recs"), brief: $("brief"), bypos: $("bypos"), search: $("search"), undo: $("undo"),
    results: $("results"), recent: $("recent"), roster: $("roster"), toast: $("toast"),
  };
  const POS_ORDER = ["QB", "RB", "WR", "TE", "DST"];
  const POS_TITLE = { DST: "D/ST" };

  let cur = null;          // last payload {state, advice, brief}
  let league = null;       // /api/league: teams, my_team, players (for name lookup)
  let byId = new Map();
  let hits = [];           // current search results
  let sel = 0;             // selected result index
  let busy = false;
  let pollTimer = null;

  // ---------- helpers ----------
  const esc = (s) => String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
  const pct = (p) => Math.round((p ?? 0) * 100);
  const posSpan = (pos) => `<span class="pos pos-${esc(pos)}">${esc(POS_TITLE[pos] || pos)}</span>`;
  const name = (id) => (byId.get(id) || {}).name || id;
  const teamShort = (t) => (t || "").length > 18 ? t.slice(0, 17) + "…" : t;

  let toastTimer = null;
  function toast(msg, kind) {
    el.toast.textContent = msg;
    el.toast.className = "toast " + (kind || "");
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => el.toast.classList.add("hidden"), kind === "ok" ? 1400 : 3000);
  }

  async function api(path, body) {
    const r = await fetch(path, body ? { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) } : undefined);
    const j = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(j.error || `${r.status} ${path}`);
    return j;
  }

  // ---------- render ----------
  function render(p) {
    cur = p;
    const st = p.state || {};
    const ad = p.advice || null;
    const me = league ? league.my_team : "";
    const onClock = !!(ad ? ad.on_clock : st.on_clock === me);

    el.app.classList.toggle("onclock", onClock);
    el.status.classList.toggle("onclock", onClock);

    if (st.complete) {
      el.pick.textContent = "DRAFT COMPLETE";
      el.turn.textContent = "";
    } else {
      el.pick.innerHTML = `LIVE PICK <span class="num">${st.live_pick}</span>${ad ? ` <span class="sep">·</span> R${ad.round}` : ""}`;
      if (onClock) {
        el.turn.textContent = "YOU'RE ON THE CLOCK";
      } else if (ad && ad.next_live_pick > 0) {
        el.turn.innerHTML = `${esc(teamShort(st.on_clock))} picking <span class="sep">·</span> you're up in <span class="num">${ad.picks_until}</span> (#${ad.next_live_pick})`;
      } else if (ad) {
        el.turn.textContent = `${teamShort(st.on_clock)} picking · no picks left`;
      } else {
        el.turn.textContent = `${teamShort(st.on_clock)} picking`;
      }
    }
    const gateTxt = ad ? [ad.gate_band, ...(ad.warnings || [])].filter(Boolean).join("  ·  ") : "";
    el.gate.textContent = gateTxt;
    el.gate.classList.toggle("hidden", !gateTxt);

    // Top recommendations. Never animated — the list must not move under the eye.
    if (!ad || !ad.top || !ad.top.length) {
      el.recs.innerHTML = `<div class="empty">${st.complete ? "Nothing left to draft." : "No recommendations yet."}</div>`;
    } else {
      el.recs.innerHTML = ad.top.slice(0, 3).map((r, i) => {
        const pl = r.player;
        const why = (r.reasons || []).map((s) => esc(s)).join(" · ");
        const badge = r.keeper_spec ? `<span class="badge spec" title="2027 keeper asset, not a 2026 contributor">2027</span>` : "";
        return `<div class="rec${r.gate_forced ? " gate" : ""}${r.keeper_spec ? " spec" : ""}" data-id="${esc(pl.id)}">
          <div class="n num">${i + 1}</div>
          <div class="who">${esc(pl.name)}${posSpan(pl.pos)}<span class="tm">${esc(pl.team)}${pl.bye ? " · bye " + pl.bye : ""}</span>${badge}</div>
          <div class="vor num">${r.vor.toFixed(1)}<small>VOR</small></div>
          <div class="why">${why.replace(/^(\d+% gone)/, "<b>$1</b>")}</div>
        </div>`;
      }).join("");
    }

    // Brief.
    if (p.brief && p.brief.text) {
      el.brief.textContent = p.brief.text.replace(/^\s*-\s*/gm, "• ");
      el.brief.classList.toggle("projected", !!p.brief.projected);
      el.brief.classList.remove("hidden");
    } else {
      el.brief.classList.add("hidden");
    }

    // Best by position.
    if (ad && ad.by_position) {
      el.bypos.innerHTML = `<div class="lbl">BEST BY POS</div>` + POS_ORDER.map((pos) => {
        const r = ad.by_position[pos];
        if (!r || !r.player) return `<span class="bp"><b class="pos-${pos}">${esc(POS_TITLE[pos] || pos)}</b><span>—</span></span>`;
        return `<span class="bp"><b class="pos-${pos}">${esc(POS_TITLE[pos] || pos)}</b>${esc(lastName(r.player.name))} <span class="num">${r.vor.toFixed(0)}/${pct(r.p_survive)}%</span></span>`;
      }).join("");
    } else {
      el.bypos.innerHTML = "";
    }

    renderRecent(st);
    renderRoster(st, ad, me);
    renderAutomation(p.automation);
  }

  // Automation freshness (§9.5): seconds since the last event from the userscript, and
  // the conflict banner when the board and a manual entry disagree.
  let autoState = null;
  function renderAutomation(a) {
    autoState = a || null;
    tickAutomation();
    const c = a && (a.last_conflict || a.conflict_note);
    el.conflict.classList.toggle("hidden", !c);
    if (c) el.conflictText.textContent = "CONFLICT — " + (a.conflict_note || `pick #${a.last_conflict.live_pick}`) + ". Fix with undo/search, then dismiss.";
  }
  function tickAutomation() {
    const a = autoState;
    if (!a || !a.seen) { el.auto.classList.add("hidden"); return; }
    const secs = Math.max(0, Math.round((Date.now() - new Date(a.last_event_at).getTime()) / 1000));
    const stale = secs > 90;
    el.auto.textContent = `auto ${secs}s ago · ${a.picks} picks` + (a.unmatched && a.unmatched.length ? ` · unmatched: ${a.unmatched.join(", ")}` : "");
    el.auto.classList.toggle("stale", stale);
    el.auto.classList.remove("hidden");
  }
  setInterval(tickAutomation, 1000);

  function lastName(n) {
    const parts = (n || "").split(" ");
    if (parts.length < 2) return n;
    // Keep suffixes readable: "Smith-Njigba", "St. Brown".
    return parts.slice(1).join(" ").replace(/^(Jr\.|Sr\.|II|III|IV)$/, parts[0]);
  }

  function renderRecent(st) {
    const picks = (st.picks || []).slice(-4).reverse();
    const me = league ? league.my_team : "";
    el.recent.innerHTML = picks.map((pk) =>
      `<div class="${pk.team === me ? "mine" : ""}"><span class="num">#${pk.live_pick}</span> ${esc(teamShort(pk.team))} → <b>${esc(name(pk.player_id))}</b>${pk.source && pk.source !== "manual" ? ` <span class="dim">(${esc(pk.source)})</span>` : ""}</div>`
    ).join("");
  }

  function renderRoster(st, ad, me) {
    const ids = (st.rosters || {})[me] || [];
    const byPos = {};
    for (const id of ids) {
      const pl = byId.get(id);
      if (!pl) continue;
      (byPos[pl.pos] = byPos[pl.pos] || []).push(pl);
    }
    // Starter slots in league order, then flex, then bench. Roster shape is fixed (§1.1).
    const starters = { QB: 1, RB: 2, WR: 2, TE: 1, DST: 1 };
    const flexElig = ["RB", "WR", "TE"];
    const slots = [];
    const used = {};
    for (const pos of POS_ORDER) {
      const list = byPos[pos] || [];
      for (let i = 0; i < (starters[pos] || 0); i++) {
        const pl = list[i];
        used[pos] = i + 1;
        slots.push({ label: pos + (starters[pos] > 1 ? i + 1 : ""), pos, pl });
      }
    }
    const surplus = flexElig.flatMap((pos) => (byPos[pos] || []).slice(starters[pos] || 0));
    for (let i = 0; i < 2; i++) slots.push({ label: "FLEX", pos: surplus[i] ? surplus[i].pos : "", pl: surplus[i] });
    const benchPl = surplus.slice(2).concat((byPos.QB || []).slice(1), (byPos.DST || []).slice(1));
    const total = ids.length;
    el.roster.innerHTML = `<div class="lbl">MY ROSTER <span class="num">${total}/17</span></div>` +
      slots.map((s) => s.pl
        ? `<span class="slot"><b class="pos-${s.pos}">${esc(s.label)}</b>${esc(lastName(s.pl.name))}</span>`
        : `<span class="slot open"><b>${esc(s.label)}</b>—</span>`).join("") +
      (benchPl.length ? `<span class="slot"><b>BN</b>${benchPl.map((p) => `<span class="pos-${p.pos}">${esc(lastName(p.name))}</span>`).join(", ")}</span>` : "");
  }

  // ---------- search ----------
  let searchSeq = 0;
  async function doSearch() {
    const q = el.search.value.trim();
    const seq = ++searchSeq;
    if (q.length < 2) { hits = []; sel = 0; renderResults(); return; }
    try {
      const r = await api(`/api/search?q=${encodeURIComponent(q)}`);
      if (seq !== searchSeq) return;
      hits = Array.isArray(r) ? r : [];
      sel = 0;
      renderResults();
    } catch (e) { /* transient; next keystroke retries */ }
  }
  function renderResults() {
    el.results.innerHTML = hits.map((p, i) =>
      `<li class="${i === sel ? "sel" : ""}" data-i="${i}"><span class="arrow">${i === sel ? "→" : "&nbsp;"}</span>${esc(p.name)} ${posSpan(p.pos)} <span class="tm">${esc(p.team)}</span><span class="adp num">ADP ${p.adp_mean ? p.adp_mean.toFixed(0) : "—"}</span></li>`
    ).join("");
  }
  function clearSearch() { el.search.value = ""; hits = []; sel = 0; renderResults(); }

  // ---------- actions ----------
  async function pick(playerId) {
    if (busy || !playerId) return;
    busy = true;
    try {
      const r = await api("/api/pick", { player_id: playerId });
      clearSearch();
      toast(`#${r.pick.live_pick} ${teamShort(r.pick.team)}: ${name(r.pick.player_id)}`, "ok");
      render(r.state);
    } catch (e) {
      toast(e.message);
    } finally {
      busy = false;
      focus();
    }
  }
  async function undo() {
    if (busy) return;
    busy = true;
    try {
      const r = await api("/api/undo", {});
      toast(`undid #${r.undone.live_pick} ${name(r.undone.player_id)}`, "dim");
      render(r.state);
    } catch (e) {
      toast(e.message);
    } finally {
      busy = false;
      focus();
    }
  }
  function focus() { el.search.focus({ preventScroll: true }); }

  el.search.addEventListener("input", doSearch);
  el.search.addEventListener("keydown", (e) => {
    if (e.key === "ArrowDown") { e.preventDefault(); if (hits.length) { sel = (sel + 1) % hits.length; renderResults(); } }
    else if (e.key === "ArrowUp") { e.preventDefault(); if (hits.length) { sel = (sel - 1 + hits.length) % hits.length; renderResults(); } }
    else if (e.key === "Enter") { e.preventDefault(); if (hits[sel]) pick(hits[sel].id); }
    else if (e.key === "Escape") { clearSearch(); }
  });
  el.results.addEventListener("click", (e) => {
    const li = e.target.closest("li[data-i]");
    if (li) pick(hits[+li.dataset.i].id);
  });
  el.recs.addEventListener("click", (e) => {
    const rec = e.target.closest(".rec[data-id]");
    if (rec && cur && cur.advice && cur.advice.on_clock) pick(rec.dataset.id);
  });
  el.undo.addEventListener("click", (e) => { e.preventDefault(); undo(); });
  el.conflictOk.addEventListener("click", async (e) => { e.preventDefault(); try { await api("/api/fandraft/resolve", {}); } catch (err) { toast(err.message); } focus(); });
  document.addEventListener("keydown", (e) => {
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "z") { e.preventDefault(); undo(); return; }
    // 1/2/3 draft a recommendation, but only when I'm on the clock and the box is empty
    // (so typing "1" into a name search can never fire a pick).
    if (/^[1-3]$/.test(e.key) && el.search.value === "" && cur && cur.advice && cur.advice.on_clock) {
      const r = (cur.advice.top || [])[+e.key - 1];
      if (r) { e.preventDefault(); pick(r.player.id); }
    }
  });
  document.addEventListener("click", (e) => {
    if (!e.target.closest("button, a, input")) focus();
  });
  window.addEventListener("focus", focus);

  // ---------- data ----------
  async function loadState() {
    try { render(await api("/api/state")); } catch (e) { toast("server unreachable: " + e.message); }
  }
  function startPolling() {
    if (pollTimer) return;
    pollTimer = setInterval(loadState, 2000);
  }
  function stopPolling() { clearInterval(pollTimer); pollTimer = null; }
  function connect() {
    const es = new EventSource("/api/stream");
    es.addEventListener("state", (ev) => {
      stopPolling();
      try { render(JSON.parse(ev.data)); } catch (e) { console.error(e); }
    });
    es.onerror = () => { startPolling(); };
  }

  (async () => {
    try {
      league = await api("/api/league");
      byId = new Map((league.players || []).map((p) => [p.id, p]));
    } catch (e) {
      toast("could not load league: " + e.message);
    }
    await loadState();
    connect();
    focus();
  })();
})();
