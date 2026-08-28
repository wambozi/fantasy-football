package engine

import (
	"fmt"
	"sort"
	"strings"

	"github.com/wambozi/draft-copilot/internal/players"
)

// Render formats advice for a terminal (simdraft, tests, debugging).
func Render(ad *Advice) string {
	var b strings.Builder
	fmt.Fprintf(&b, "LIVE PICK %d (round %d)  on_clock=%v  next=#%d  picks_until=%d  mode=%s\n",
		ad.LivePick, ad.Round, ad.OnClock, ad.NextLivePick, ad.PicksUntil, ad.ProjMode)
	if ad.GateBand != "" {
		fmt.Fprintf(&b, "  !! %s\n", ad.GateBand)
	}
	for _, w := range ad.Warnings {
		fmt.Fprintf(&b, "  warn: %s\n", w)
	}
	for i, r := range ad.Top {
		fmt.Fprintf(&b, "  %d  %-24s %-3s %-4s score %6.1f  VOR %6.1f  surv %3.0f%%  %s\n", i+1, r.Player.Name, r.Player.Pos, r.Player.Team,
			r.Score, r.VOR, r.PSurvive*100, strings.Join(r.Reasons, " · "))
	}
	var poss []string
	for pos := range ad.ByPosition {
		poss = append(poss, string(pos))
	}
	sort.Strings(poss)
	b.WriteString("  by pos:")
	for _, pos := range poss {
		r := ad.ByPosition[players.Position(pos)]
		fmt.Fprintf(&b, "  %s %s (%.0f/%.0f%%)", pos, r.Player.Name, r.VOR, r.PSurvive*100)
	}
	b.WriteString("\n  demand:")
	for _, pos := range poss {
		fmt.Fprintf(&b, "  %s %.1f→repl %.0f", pos, ad.Demand[players.Position(pos)], ad.Replacement[players.Position(pos)])
	}
	b.WriteString("\n")
	return b.String()
}
