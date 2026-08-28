package players

import (
	"sort"
	"strings"
)

// Search ranks draftable players matching q. Matching is on the normalised name:
// name prefix beats word prefix beats in-order subsequence (so "jsn" finds
// Jaxon Smith-Njigba and "jud" finds Judkins). Ties break on ADP so the player you
// almost certainly mean — the one about to be drafted — is first. exclude filters
// out taken players.
func (p *Pool) Search(q string, exclude func(*Player) bool, limit int) []*Player {
	q = NameKey(q)
	if q == "" {
		return nil
	}
	type hit struct {
		p    *Player
		rank int
	}
	var hits []hit
	for _, pl := range p.Players {
		if !pl.Pos.Draftable() || (exclude != nil && exclude(pl)) {
			continue
		}
		r := matchRank(NameKey(pl.Name), q)
		if r == 0 && len(q) >= 2 {
			// Allow "TEAM" or "POS TEAM" style queries for D/ST, e.g. "dst phi".
			r = matchRank(strings.ToLower(pl.Team+" "+string(pl.Pos)), q)
		}
		if r > 0 {
			hits = append(hits, hit{pl, r})
		}
	}
	sort.SliceStable(hits, func(a, b int) bool {
		if hits[a].rank != hits[b].rank {
			return hits[a].rank > hits[b].rank
		}
		return adpOrLast(hits[a].p) < adpOrLast(hits[b].p)
	})
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]*Player, len(hits))
	for i, h := range hits {
		out[i] = h.p
	}
	return out
}

func adpOrLast(p *Player) float64 {
	if p.ADPMean <= 0 {
		return 1e9
	}
	return p.ADPMean
}

// matchRank: 3 = name prefix, 2 = any word prefix / initials, 1 = subsequence, 0 = miss.
func matchRank(name, q string) int {
	if strings.HasPrefix(name, q) {
		return 3
	}
	words := strings.Fields(name)
	initials := ""
	for _, w := range words {
		if strings.HasPrefix(w, q) {
			return 2
		}
		initials += w[:1]
	}
	if strings.HasPrefix(initials, q) {
		return 2
	}
	// Subsequence over the compacted name.
	i := 0
	for _, c := range strings.ReplaceAll(name, " ", "") {
		if i < len(q) && byte(c) == q[i] {
			i++
		}
	}
	if i == len(q) {
		return 1
	}
	return 0
}
