// The Draft Copilot — web UI. Vanilla ES2020, no framework, no build step.
//
// Data flow: GET /api/league once (teams, slots, players, roster shape), GET /api/state on
// load, then /api/stream SSE pushes the whole payload on every version bump. If the
// stream drops we poll every 2s until it comes back.
//
// Every visual decision lives in draft-copilot.css / broadsheet.css. This file carries no
// colors and no font families — it only chooses class names.
(() => {
  "use strict";
  const $ = (id) => document.getElementById(id);
  const el = {
    body: document.body,
    dateline: $("dateline"), auto: $("auto"), density: $("density"), undo: $("undo"),
    status: $("status"), statusKicker: $("status-kicker"), statusFigure: $("status-figure"),
    untilKicker: $("until-kicker"), untilFigure: $("until-figure"),
    gate: $("gate"), conflict: $("conflict"), conflictText: $("conflict-text"), conflictOk: $("conflict-ok"),
    read: $("read"), search: $("search"), hits: $("hits"),
    tabs: Array.from(document.querySelectorAll(".dc-tab")),
    viewShortlist: $("view-shortlist"), viewLeague: $("view-league"),
    cards: $("cards"), bypos: $("bypos"), brief: $("brief"), briefKicker: $("brief-kicker"), briefBody: $("brief-body"),
    needsRows: $("needs-rows"), likelyHead: $("likely-head"),
    rosterCount: $("roster-count"), roster: $("roster"),
    picksIn: $("picks-in"), ondeck: $("ondeck"), board: $("board"), toast: $("toast"),
  };
  const ORDER = ["QB", "RB", "WR", "TE", "DST"];
  const GRID_POS = ["QB", "RB", "WR", "TE"];
  const POS_TITLE = { DST: "D/ST" };

  // ---------- state ----------
  let cur = null;        // last /api/state payload: {state, advice, brief, automation}
  let league = null;     // /api/league: teams, my_team, slots, players, roster, need
  let byId = new Map();
  let slotByLive = new Map(); // live pick -> slot
  let hits = [];
  let sel = 0;
  let view = "shortlist";
  let density = "dense";
  let busy = false;
  let pollTimer = null;
  let autoState = null;

  // ---------- helpers ----------
  const esc = (s) => String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
  const pct = (p) => Math.round((p ?? 0) * 100);
  const f1 = (v) => (v == null ? "—" : (+v).toFixed(1));
  const posTitle = (pos) => POS_TITLE[pos] || pos;
  const name = (id) => (byId.get(id) || {}).name || id;
  const shortTeam = (t) => ((t || "").length > 22 ? t.slice(0, 21) + "…" : t || "");
  function lastName(n) {
    const parts = (n || "").split(" ");
    if (parts.length < 2) return n;
    const rest = parts.slice(1).join(" ");
    return /^(Jr\.|Sr\.|II|III|IV)$/.test(rest) ? parts[0] : rest;
  }
  const roster = () => (league && league.roster) || { starters: { QB: 1, RB: 2, WR: 2, TE: 1, DST: 1 }, flex: { count: 2, eligible: ["RB", "WR", "TE"] }, max: { QB: 4, RB: 9, WR: 8, TE: 3, DST: 3 }, total_spots: 17 };
  const need = () => (league && league.need) || { starter_open: 1.8, flex_open: 1.25, full: 0.6, at_max: 0, dst_before_round: 15, dst_early_mult: 0.05 };

  let toastTimer = null;
  function toast(msg, kind) {
    el.toast.textContent = msg;
    el.toast.className = "dc-toast " + (kind === "ok" ? "is-ok" : kind === "err" ? "is-err" : "");
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => el.toast.classList.add("hidden"), kind === "ok" ? 1400 : 3000);
  }

  async function api(path, body) {
    const r = await fetch(path, body ? { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) } : undefined);
    const j = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(j.error || `${r.status} ${path}`);
    return j;
  }

  // ---------- roster arithmetic (a port of internal/engine/roster.go) ----------
  function counts(team) {
    const c = { QB: 0, RB: 0, WR: 0, TE: 0, DST: 0 };
    const ids = ((cur && cur.state && cur.state.rosters) || {})[team] || [];
    for (const id of ids) {
      const p = byId.get(id);
      if (p && c[p.pos] != null) c[p.pos]++;
    }
    return c;
  }
  const starterOpen = (c, pos) => Math.max(0, (roster().starters[pos] || 0) - c[pos]);
  function flexOpen(c) {
    let used = 0;
    for (const pos of roster().flex.eligible) used += Math.max(0, c[pos] - (roster().starters[pos] || 0));
    return Math.max(0, roster().flex.count - used);
  }
  function openStarters(c) {
    let n = flexOpen(c);
    for (const pos of ORDER) n += starterOpen(c, pos);
    return n;
  }
  const isFlex = (pos) => roster().flex.eligible.includes(pos);
  function status(c, pos) {
    const max = roster().max[pos];
    if (max != null && c[pos] >= max) return "max";
    if (starterOpen(c, pos) > 0) return "open";
    if (flexOpen(c) > 0 && isFlex(pos)) return "flex";
    return "set";
  }
  function needMult(c, pos, round) {
    const n = need();
    const st = status(c, pos);
    let m = st === "max" ? n.at_max : st === "open" ? n.starter_open : st === "flex" ? n.flex_open : n.full;
    if (pos === "DST" && round < n.dst_before_round) m *= n.dst_early_mult;
    return m;
  }

  // Likely next move for a team. Preferred: advice.pos_by_team, the Monte Carlo share
  // of sims in which that team's next pick went to each position. For teams outside the
  // simulated window (they pick after my next turn) fall back to the prototype's
  // need × best-available-VOR proxy — a heuristic, not a simulation, and labelled so.
  function predict(team, round) {
    const ad = cur && cur.advice;
    const sim = ad && ad.pos_by_team && ad.pos_by_team[team];
    if (sim) {
      const ws = Object.entries(sim).map(([pos, share]) => ({ pos, share })).sort((a, b) => b.share - a.share);
      return ws.length ? { pos: ws[0].pos, share: ws[0].share, second: ws[1] ? ws[1].pos : null, secondShare: ws[1] ? ws[1].share : 0, simulated: true } : null;
    }
    if (!ad || !ad.by_position) return null;
    const c = counts(team);
    const ws = [];
    let total = 0;
    for (const pos of ORDER) {
      const b = ad.by_position[pos];
      if (!b || !b.player) continue;
      const w = needMult(c, pos, round) * Math.max(0, b.vor);
      total += w;
      ws.push({ pos, w });
    }
    if (!ws.length || total <= 0) return null;
    ws.sort((a, b) => b.w - a.w);
    return { pos: ws[0].pos, share: ws[0].w / total, second: ws[1] ? ws[1].pos : null, secondShare: ws[1] ? ws[1].w / total : 0, simulated: false };
  }

  // The slot that will fill live pick L (null = none), and the team on it.
  const slotAt = (live) => slotByLive.get(live) || null;
  const teamAt = (live) => (slotAt(live) || {}).team || "";
  const nextLiveFor = (team, from) => {
    for (let l = from; l <= (league ? league.num_live : 0); l++) if (teamAt(l) === team) return l;
    return 0;
  };
  const roundOf = (live) => (slotAt(live) || {}).round || 0;

  // ---------- render ----------
  function render(p) {
    cur = p;
    const st = p.state || {};
    const ad = p.advice || null;
    const me = league ? league.my_team : "";
    const onClock = !!(ad ? ad.on_clock : st.on_clock === me);
    const live = st.live_pick || 0;
    const next = ad ? ad.next_live_pick : 0;
    const myCounts = counts(me);

    renderMasthead(st, ad);
    renderStatus(st, ad, onClock, live, next);
    renderGate(ad);
    renderRead(st, ad, onClock, live, next);
    renderCards(ad, onClock, myCounts, next || live);
    renderByPos(ad, myCounts);
    renderBrief(p, live);
    renderLeague(st, ad, me, live, next);
    renderRoster(st, me, myCounts);
    renderRail(st, ad, me, live, next);
    renderAutomation(p.automation);
  }

  function renderMasthead(st, ad) {
    const bits = [];
    if (ad && ad.round) bits.push(`Round ${ad.round}`);
    if (st.live_pick) bits.push(`Live pick <span class="dc-num">${st.live_pick}</span>`);
    if (league) {
      bits.push(`${league.teams.length} teams · full PPR`);
      bits.push(`Keeper · ${league.rounds} rounds`);
      bits.push(esc(league.my_team));
    }
    el.dateline.innerHTML = bits.map((b) => `<span>${b}</span>`).join("");
  }

  function renderStatus(st, ad, onClock, live, next) {
    el.status.classList.toggle("is-mine", onClock);
    el.status.classList.toggle("is-complete", !!st.complete);
    if (st.complete) {
      el.statusKicker.textContent = "The draft";
      el.statusFigure.textContent = "Draft complete";
      el.untilKicker.textContent = "";
      el.untilFigure.textContent = "";
      return;
    }
    if (onClock) {
      const lead = ad && ad.top && ad.top[0];
      el.statusKicker.textContent = "Your pick";
      el.statusFigure.innerHTML = `You're up — #<span class="dc-num">${live}</span>`;
      el.untilKicker.textContent = "Board leader";
      el.untilFigure.innerHTML = lead ? `<span class="dc-num">${f1(lead.vor)}</span> VOR` : "—";
    } else {
      el.statusKicker.textContent = "On the clock";
      el.statusFigure.textContent = shortTeam(st.on_clock);
      el.untilKicker.textContent = next > 0 ? "You're up in" : "";
      el.untilFigure.innerHTML = next > 0 ? `<span class="dc-num">${ad ? ad.picks_until : "—"}</span> · #<span class="dc-num">${next}</span>` : "No picks left";
    }
  }

  function renderGate(ad) {
    const txt = ad ? [ad.gate_band, ...(ad.warnings || [])].filter(Boolean).join("  ·  ") : "";
    el.gate.textContent = txt;
    el.gate.classList.toggle("hidden", !txt);
  }

  // The read: what comes off the board before my next turn. Built from the per-team
  // predictions (Monte Carlo shares when available), never from Claude.
  function renderRead(st, ad, onClock, live, next) {
    if (!ad || st.complete) { el.read.textContent = st.complete ? "The board is full." : ""; return; }
    if (onClock) {
      const lead = ad.top && ad.top[0];
      el.read.textContent = `You are on the clock at #${live}.` + (lead ? ` ${lead.player.name} leads the board at ${f1(lead.vor)} VOR.` : "");
      return;
    }
    if (!(next > 0)) { el.read.textContent = "No picks left."; return; }
    const seq = [];
    const exposed = [];
    for (let l = live; l < next; l++) {
      const pred = predict(teamAt(l), roundOf(l));
      seq.push(pred ? pred.pos : "?");
      if (pred && ad.by_position && ad.by_position[pred.pos] && exposed.length < 2) {
        const nm = ad.by_position[pred.pos].player.name;
        if (!exposed.includes(nm)) exposed.push(nm);
      }
    }
    // One entry per intervening pick, in order — but a long snake stretch reads as
    // runs, so consecutive repeats collapse to "RB ×5".
    const runs = [];
    for (const pos of seq) {
      const last = runs[runs.length - 1];
      if (last && last.pos === pos) last.n++; else runs.push({ pos, n: 1 });
    }
    const seqText = runs.map((r) => (r.n > 1 ? `${r.pos} ×${r.n}` : r.pos)).join(" then ");
    // Keeper slots sitting between now and my turn are already committed.
    const keepers = keepersBetween(live, next);
    const named = keepers.slice(0, 2).map((s) => `${lastName(s.keeper_name)} is a keeper locked in round ${s.round}`);
    const more = keepers.length > 2 ? ` and ${keepers.length - 2} more keeper slots sit in between` : "";
    el.read.textContent = `Expect ${seqText || "nothing"} off the board before #${next}` +
      (exposed.length ? ` — ${exposed.join(" and ")} ${exposed.length > 1 ? "are the exposed names" : "is the exposed name"}` : "") +
      (named.length ? `. ${named.join("; ")}${more}.` : ".");
  }

  // Keeper slots whose overall position falls between live pick a (inclusive) and b.
  function keepersBetween(a, b) {
    const sa = slotAt(a), sb = slotAt(b);
    if (!sa || !sb || !league) return [];
    return league.slots.filter((s) => s.keeper_id && s.overall > sa.overall && s.overall < sb.overall);
  }

  function renderCards(ad, onClock, myCounts, nextPick) {
    const top = (ad && ad.top) || [];
    if (!top.length) {
      el.cards.innerHTML = `<p class="dc-card-why">${cur && cur.state && cur.state.complete ? "Nothing left to draft." : "No recommendations yet."}</p>`;
      return;
    }
    el.cards.innerHTML = top.slice(0, 3).map((r, i) => {
      const pl = r.player;
      const gone = pct(1 - r.p_survive);
      const risk = gone >= 45 ? "dc-risk-high" : gone >= 20 ? "dc-risk-mid" : "dc-risk-low";
      const fill = status(myCounts, pl.pos);
      const slot = fill === "open" ? `${posTitle(pl.pos)} starter` : fill === "flex" ? "Flex slot" : "Bench";
      const flag = r.gate_forced ? `<span class="dc-card-flag is-gate">Gate forced</span>` : i === 0 ? `<span class="dc-card-flag">Board leader</span>` : r.keeper_spec ? `<span class="dc-card-flag">2027 asset</span>` : "";
      const why = (r.reasons || []).filter((x) => !/^\d+% gone by/.test(x)).map(esc).join(" · ");
      return `<article class="card dc-card${r.gate_forced ? " is-gate" : ""}" data-id="${esc(pl.id)}">
        <div class="dc-card-head"><span class="dc-card-rank dc-num">${i + 1}</span>${flag}<span class="dc-card-slot">${esc(slot)}</span></div>
        <h3 class="dc-card-name">${esc(pl.name)}</h3>
        <div class="dc-card-meta">${esc(posTitle(pl.pos))} · ${esc(pl.team)}${pl.bye ? ` · bye ${pl.bye}` : ""}</div>
        <div class="dc-card-vor"><b>${f1(r.vor)}</b><small>vor</small></div>
        <div class="dc-card-gone ${risk}"><b>${gone}%</b><small>gone by #${nextPick}</small></div>
        <div class="dc-meter ${risk}"><span style="width:${gone}%"></span></div>
        <p class="dc-card-why">${why}</p>
        <div class="dc-card-stats"><span>ADP ${Math.round(pl.adp_mean)}</span><span>σ ${f1(pl.adp_stddev)}</span><span>regret ${f1(r.regret)}</span><span>score ${f1(r.score)}</span></div>
        ${onClock ? `<button class="btn btn-primary btn-block dc-card-btn" type="button" data-draft="${esc(pl.id)}">Draft — press ${i + 1}</button>` : ""}
      </article>`;
    }).join("");
  }

  function renderByPos(ad, myCounts) {
    if (!ad || !ad.by_position) { el.bypos.innerHTML = ""; return; }
    el.bypos.innerHTML = ORDER.map((pos) => {
      const r = ad.by_position[pos];
      const st = status(myCounts, pos);
      const cls = st === "open" ? "is-open" : st === "flex" ? "is-flex" : "is-set";
      if (!r || !r.player) return `<div><div class="dc-bypos-pos ${cls}">${esc(posTitle(pos))}</div><div class="dc-bypos-name">—</div><div class="dc-bypos-stat"></div></div>`;
      return `<div><div class="dc-bypos-pos ${cls}">${esc(posTitle(pos))}</div><div class="dc-bypos-name" title="${esc(r.player.name)}">${esc(lastName(r.player.name))}</div><div class="dc-bypos-stat">${Math.round(r.vor)} vor · adp ${Math.round(r.player.adp_mean)} · ${pct(r.p_survive)}% survives</div></div>`;
    }).join("");
  }

  function renderBrief(p, live) {
    const b = p.brief;
    if (!b || !b.text) { el.brief.classList.add("hidden"); return; }
    el.briefKicker.textContent = b.projected ? "Claude ≈ projected" : `Claude — the read at #${b.live_pick || live}`;
    el.briefBody.textContent = b.text.replace(/^\s*-\s*/gm, "• ");
    el.brief.classList.remove("hidden");
  }

  function renderLeague(st, ad, me, live, next) {
    if (!league) { el.needsRows.innerHTML = ""; return; }
    const anySim = ad && ad.pos_by_team && Object.keys(ad.pos_by_team).length > 0;
    el.likelyHead.textContent = anySim ? "Likely next move" : "Likely next move (heuristic)";
    el.needsRows.innerHTML = league.teams.map((team) => {
      const c = counts(team);
      const nx = nextLiveFor(team, live);
      const mine = team === me;
      const upcoming = !mine && nx > 0 && next > 0 && nx < next;
      const pred = mine ? null : predict(team, roundOf(nx || live));
      const missing = GRID_POS.filter((pos) => starterOpen(c, pos) > 0);
      const surplus = roster().flex.eligible.map((pos) => ({ pos, n: c[pos] - (roster().starters[pos] || 0) })).filter((x) => x.n > 0).sort((a, b) => b.n - a.n);
      const parts = [];
      if (missing.length) parts.push(`needs ${missing.join(", ")}`);
      else parts.push(flexOpen(c) > 0 ? "starters set, flex live" : "starters and flex full");
      if (surplus.length) parts.push(surplus.map((x) => `+${x.n} ${x.pos}`).join(", ") + " past the starters");
      if (starterOpen(c, "DST") > 0 && roundOf(live) >= need().dst_before_round) parts.push("still needs D/ST");
      const note = mine ? "your seat" : parts.join(" · ");
      const likely = !pred ? "—" : `${pred.pos} ${pct(pred.share)}%` + (pred.second ? `  ·  ${pred.second} ${pct(pred.secondShare)}%` : "") + (pred.simulated ? "" : " ≈");
      const cells = GRID_POS.map((pos) => {
        const s = status(c, pos);
        return `<div class="dc-needs-cell is-${s}"><b>${c[pos]}</b><small>${s}</small></div>`;
      }).join("");
      return `<div class="dc-needs-row${mine ? " is-mine" : upcoming ? " is-upcoming" : ""}">
        <div class="dc-needs-next${mine ? " is-mine" : upcoming ? " is-upcoming" : ""}">${nx ? "#" + nx : "—"}</div>
        <div class="dc-needs-team" title="${esc(team)}">${esc(shortTeam(team))}</div>
        ${cells}
        <div class="dc-needs-open">${openStarters(c)}</div>
        <div class="dc-needs-move"><div class="dc-needs-likely">${esc(likely)}</div><div class="dc-needs-note">${esc(note)}</div></div>
      </div>`;
    }).join("");
  }

  function renderRoster(st, me, c) {
    const ids = ((st.rosters || {})[me] || []).map((id) => byId.get(id)).filter(Boolean);
    const byPos = {};
    for (const p of ids) (byPos[p.pos] = byPos[p.pos] || []).push(p);
    const slots = [];
    for (const pos of ORDER) {
      const n = roster().starters[pos] || 0;
      for (let i = 0; i < n; i++) slots.push({ label: pos + (n > 1 ? i + 1 : ""), pl: (byPos[pos] || [])[i] });
    }
    const surplus = roster().flex.eligible.flatMap((pos) => (byPos[pos] || []).slice(roster().starters[pos] || 0));
    for (let i = 0; i < roster().flex.count; i++) slots.push({ label: "Flex", pl: surplus[i] });
    const bench = surplus.slice(roster().flex.count).concat((byPos.QB || []).slice(roster().starters.QB || 0), (byPos.DST || []).slice(roster().starters.DST || 0));
    el.rosterCount.textContent = `${ids.length} of ${roster().total_spots} · ${openStarters(c)} starters open`;
    el.roster.innerHTML = slots.map((s) => s.pl
      ? `<div class="dc-slot"><b>${esc(s.label)}</b><span title="${esc(s.pl.name)}">${esc(lastName(s.pl.name))}</span></div>`
      : `<div class="dc-slot is-open"><b>${esc(s.label)}</b><span>open</span></div>`).join("") +
      bench.map((p) => `<div class="dc-slot"><b>Bench</b><span title="${esc(p.name)}">${esc(lastName(p.name))}</span></div>`).join("");
  }

  // The rail: on deck (next five slots, keepers included) and every slot already
  // passed, newest first, grouped by round. Keeper slots come from league.slots; live
  // picks from state.picks. Pick numbers are live-pick numbers, as everywhere else in
  // this app; keeper rows carry no number.
  function renderRail(st, ad, me, live, next) {
    const picks = st.picks || [];
    el.picksIn.textContent = `${picks.length} picks in`;
    if (!league) { el.ondeck.innerHTML = ""; el.board.innerHTML = ""; return; }
    const byLive = new Map(picks.map((pk) => [pk.live_pick, pk]));
    const curSlot = slotAt(live);
    const curOverall = curSlot ? curSlot.overall : league.slots.length + 1;

    // On deck: the next five slots from the current one, keepers interleaved.
    const deck = league.slots.filter((s) => s.overall >= curOverall).slice(0, 5);
    el.ondeck.innerHTML = deck.map((s) => {
      const mine = s.team === me;
      const now = s.live_pick === live;
      let tag, tagCls;
      if (s.keeper_id) { tag = `keeper: ${lastName(name(s.keeper_id))}`; tagCls = "is-known"; }
      else if (mine) { tag = "your pick"; tagCls = "is-mine"; }
      else { const pr = predict(s.team, s.round); tag = pr ? `likely ${pr.pos}` : ""; tagCls = ""; }
      return `<div class="dc-ondeck-row${mine ? " is-mine" : now ? " is-now" : ""}">
        <div class="dc-ondeck-no">${s.live_pick ? "#" + s.live_pick : "—"}</div>
        <div class="dc-ondeck-team" title="${esc(s.team)}">${mine ? "You — " : ""}${esc(shortTeam(s.team))}</div>
        <div class="dc-ondeck-tag ${tagCls}">${esc(tag)}</div>
      </div>`;
    }).join("");

    // The board: every slot before the current one, newest first.
    const made = league.slots.filter((s) => s.overall < curOverall && (s.keeper_id || byLive.has(s.live_pick))).sort((a, b) => b.overall - a.overall);
    let prevRound = null;
    el.board.innerHTML = made.map((s) => {
      const keeper = !!s.keeper_id;
      const pk = keeper ? null : byLive.get(s.live_pick);
      const pl = byId.get(keeper ? s.keeper_id : pk.player_id) || { name: keeper ? s.keeper_name : pk.player_id, pos: keeper ? s.keeper_pos : "" };
      const divider = s.round !== prevRound ? `<div class="dc-round"><span>Round ${s.round}</span><i></i></div>` : "";
      prevRound = s.round;
      // ADP delta is measured against the overall board position: ADP is an overall
      // pick number, and keeper slots occupy board positions too.
      const d = pl.adp_mean ? Math.round(s.overall - pl.adp_mean) : 0; // + = fell past ADP
      let delta, dcls;
      if (keeper) { delta = "kept"; dcls = "is-keeper"; }
      else if (d >= 3) { delta = `+${d} value`; dcls = "is-value"; }
      else if (d <= -3) { delta = `−${Math.abs(d)} reach`; dcls = "is-reach"; }
      else { delta = "at adp"; dcls = "is-flat"; }
      const recorded = pk && cur.pick_vor && cur.pick_vor[pk.live_pick];
      const vor = recorded != null ? recorded : vorOf(pl, ad);
      return `${divider}<div class="dc-pick${s.team === me ? " is-mine" : ""}">
        <div class="dc-pick-no">${s.live_pick ? "#" + s.live_pick : "—"}</div>
        <div class="dc-pick-body">
          <div class="dc-pick-name" title="${esc(pl.name)}">${esc(pl.name)}<small>${esc(posTitle(pl.pos))}</small></div>
          <div class="dc-pick-team">${esc(shortTeam(s.team))}${keeper ? " · keeper" : ""}${pk && pk.source && pk.source !== "manual" ? ` · ${esc(pk.source)}` : ""}</div>
        </div>
        <div class="dc-pick-right"><div class="dc-pick-vor">${vor == null ? "—" : f1(vor)}</div><div class="dc-pick-delta ${dcls}">${delta}</div></div>
      </div>`;
    }).join("");
  }

  // Rail and search-hit VOR: projection over the position's waiver level — what the
  // player is worth over free agency. The server records this at pick time
  // (payload.pick_vor); for picks made before this process started, or for hits, it
  // is recomputed against the current waiver level.
  function vorOf(pl, ad) {
    if (!ad || !ad.waiver || !pl.proj_points) return null;
    const w = ad.waiver[pl.pos];
    return w == null ? null : Math.max(0, pl.proj_points - w);
  }

  // ---------- automation (spec §9.5) ----------
  function renderAutomation(a) {
    autoState = a || null;
    tickAutomation();
    const c = a && (a.last_conflict || a.conflict_note);
    el.conflict.classList.toggle("hidden", !c);
    if (c) el.conflictText.textContent = `Conflict — ${a.conflict_note || `pick #${a.last_conflict.live_pick}`}. Fix with undo or search, then dismiss.`;
  }
  function tickAutomation() {
    const a = autoState;
    if (!a || !a.seen) { el.auto.classList.add("hidden"); return; }
    const secs = Math.max(0, Math.round((Date.now() - new Date(a.last_event_at).getTime()) / 1000));
    el.auto.textContent = `auto ${secs}s ago · ${a.picks} picks` + (a.unmatched && a.unmatched.length ? ` · unmatched: ${a.unmatched.join(", ")}` : "");
    el.auto.classList.toggle("is-stale", secs > 90);
    el.auto.classList.remove("hidden");
  }
  setInterval(tickAutomation, 1000);

  // ---------- search ----------
  let searchSeq = 0;
  async function doSearch() {
    const q = el.search.value.trim();
    const seq = ++searchSeq;
    if (q.length < 2) { hits = []; sel = 0; renderHits(); return; }
    try {
      const r = await api(`/api/search?q=${encodeURIComponent(q)}`);
      if (seq !== searchSeq) return;
      hits = (Array.isArray(r) ? r : []).slice(0, 6);
      sel = 0;
      renderHits();
    } catch (e) { /* transient; the next keystroke retries */ }
  }
  function renderHits() {
    el.hits.classList.toggle("hidden", !hits.length);
    const ad = cur && cur.advice;
    el.hits.innerHTML = hits.map((p, i) => {
      const v = vorOf(p, ad);
      return `<div class="dc-hit${i === sel ? " is-selected" : ""}" role="option" data-i="${i}">
        <div class="dc-hit-caret">${i === sel ? "→" : ""}</div>
        <div class="dc-hit-name">${esc(p.name)}</div>
        <div class="dc-hit-meta">${esc(posTitle(p.pos))} · ${esc(p.team)}</div>
        <div class="dc-hit-num">ADP ${p.adp_mean ? Math.round(p.adp_mean) : "—"}</div>
        <div class="dc-hit-num is-right">${v == null ? "—" : f1(v)} VOR</div>
      </div>`;
    }).join("");
  }
  function clearSearch() { el.search.value = ""; hits = []; sel = 0; renderHits(); }

  // ---------- actions ----------
  function focus() { el.search.focus({ preventScroll: true }); }
  async function pick(playerId) {
    if (busy || !playerId) return;
    busy = true;
    try {
      const r = await api("/api/pick", { player_id: playerId });
      clearSearch();
      toast(`#${r.pick.live_pick} ${shortTeam(r.pick.team)}: ${name(r.pick.player_id)}`, "ok");
      render(r.state);
    } catch (e) {
      toast(e.message, "err");
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
      toast(`Undid #${r.undone.live_pick} ${name(r.undone.player_id)}`);
      render(r.state);
    } catch (e) {
      toast(e.message, "err");
    } finally {
      busy = false;
      focus();
    }
  }
  function setView(v) {
    view = v;
    el.viewShortlist.classList.toggle("hidden", v !== "shortlist");
    el.viewLeague.classList.toggle("hidden", v !== "league");
    for (const t of el.tabs) t.setAttribute("aria-selected", String(t.dataset.view === v));
  }
  function setDensity(d, persist) {
    density = d === "airy" ? "airy" : "dense";
    el.body.dataset.density = density;
    for (const input of el.density.querySelectorAll("input")) input.checked = input.value === density;
    if (persist) { try { localStorage.setItem("dc.density", density); } catch (_) { /* private mode */ } }
  }

  el.search.addEventListener("input", doSearch);
  el.search.addEventListener("keydown", (e) => {
    if (e.key === "ArrowDown") { e.preventDefault(); if (hits.length) { sel = (sel + 1) % hits.length; renderHits(); } }
    else if (e.key === "ArrowUp") { e.preventDefault(); if (hits.length) { sel = (sel - 1 + hits.length) % hits.length; renderHits(); } }
    else if (e.key === "Enter") { e.preventDefault(); if (hits[sel]) pick(hits[sel].id); }
    else if (e.key === "Escape") { clearSearch(); }
  });
  el.hits.addEventListener("click", (e) => {
    const row = e.target.closest(".dc-hit[data-i]");
    if (row) pick(hits[+row.dataset.i].id);
  });
  el.cards.addEventListener("click", (e) => {
    const btn = e.target.closest("[data-draft]");
    if (btn && cur && cur.advice && cur.advice.on_clock) pick(btn.dataset.draft);
  });
  el.undo.addEventListener("click", (e) => { e.preventDefault(); undo(); });
  el.conflictOk.addEventListener("click", async (e) => { e.preventDefault(); try { await api("/api/fandraft/resolve", {}); } catch (err) { toast(err.message, "err"); } focus(); });
  for (const t of el.tabs) t.addEventListener("click", () => { setView(t.dataset.view); focus(); });
  el.density.addEventListener("change", (e) => { if (e.target.name === "density") { setDensity(e.target.value, true); focus(); } });

  document.addEventListener("keydown", (e) => {
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "z") { e.preventDefault(); undo(); return; }
    const tag = (e.target.tagName || "").toLowerCase();
    const inField = tag === "input" || tag === "textarea";
    if (e.key.toLowerCase() === "g" && !inField && !e.metaKey && !e.ctrlKey && !e.altKey) { e.preventDefault(); setView(view === "shortlist" ? "league" : "shortlist"); return; }
    // 1/2/3 draft a card, but only when I'm on the clock and the search field is empty —
    // typing a jersey number into a name search must never fire a pick.
    if (/^[1-3]$/.test(e.key) && el.search.value === "" && cur && cur.advice && cur.advice.on_clock) {
      const r = (cur.advice.top || [])[+e.key - 1];
      if (r) { e.preventDefault(); pick(r.player.id); }
    }
  });
  document.addEventListener("click", (e) => {
    if (!e.target.closest("button, a, input, label, .dc-hit, .dc-tab")) focus();
  });
  window.addEventListener("focus", focus);

  // ---------- data ----------
  async function loadState() {
    try { render(await api("/api/state")); } catch (e) { toast("Server unreachable: " + e.message, "err"); }
  }
  function startPolling() { if (!pollTimer) pollTimer = setInterval(loadState, 2000); }
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
    let stored = null;
    try { stored = localStorage.getItem("dc.density"); } catch (_) { /* unavailable */ }
    setDensity(stored || "dense", false);
    setView(location.hash === "#league" ? "league" : "shortlist");
    try {
      league = await api("/api/league");
      byId = new Map((league.players || []).map((p) => [p.id, p]));
      slotByLive = new Map((league.slots || []).filter((s) => s.live_pick > 0).map((s) => [s.live_pick, s]));
    } catch (e) {
      toast("Could not load league: " + e.message, "err");
    }
    await loadState();
    connect();
    focus();
  })();
})();
