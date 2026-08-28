package main

import (
	"log/slog"
	"math"
	"strings"
	"testing"

	"github.com/wambozi/draft-copilot/internal/players"
	"github.com/wambozi/draft-copilot/internal/strategy"
)

func testCfg() strategy.ADP {
	c := strategy.ADP{
		Sources: map[string]strategy.Source{
			"A": {Weight: 1, Sigma: true}, "B": {Weight: 1, Sigma: true}, "C": {Weight: 1, Sigma: true},
			"D": {Weight: 0.4, Sigma: false}, "E": {Weight: 1, Sigma: true, PosWeight: map[players.Position]float64{players.TE: 0.3}},
		},
		MinSources: 3, SigmaFloor: 3, SigmaMADScale: 1.4826,
		PosMap: map[string]string{"TD": "DST", "PK": "K", "CB": "WR"},
	}
	c.ThinSigma.Min, c.ThinSigma.Frac = 12, 0.15
	return c
}

func TestWeightedMedian(t *testing.T) {
	cases := []struct {
		vals, w []float64
		want    float64
	}{
		{[]float64{1, 2, 3}, []float64{1, 1, 1}, 2},
		{[]float64{3, 1, 2}, []float64{1, 1, 1}, 2},
		{[]float64{10, 20, 30, 40}, []float64{1, 1, 1, 1}, 20},
		{[]float64{10, 20, 30, 40}, []float64{0.1, 0.1, 1, 1}, 30}, // heavy tail wins
		{[]float64{5}, []float64{1}, 5},
	}
	for _, c := range cases {
		if got := WeightedMedian(c.vals, c.w); got != c.want {
			t.Errorf("WeightedMedian(%v,%v)=%v want %v", c.vals, c.w, got, c.want)
		}
	}
}

func TestSummarize(t *testing.T) {
	cfg := testCfg()
	cases := []struct {
		name            string
		vals, w, sig    []float64
		consensus       float64
		wantMean, wantS float64
	}{
		{"agreement floors sigma", []float64{10, 10, 10, 10}, []float64{1, 1, 1, 1}, []float64{10, 10, 10, 10}, 10, 10, 3},
		{"spread", []float64{10, 20, 30, 40, 50}, []float64{1, 1, 1, 1, 1}, []float64{10, 20, 30, 40, 50}, 30, 30, 1.4826 * 10},
		{"thin -> consensus + thin sigma", nil, nil, nil, 200, 200, 30},
		{"thin small mean -> min 12", []float64{20, 22}, []float64{1, 1}, []float64{20, 22}, 21, 20, 12},
		{"enough vals but bestball-only sigma", []float64{10, 12, 14}, []float64{0.4, 0.4, 0.4}, nil, 12, 12, 12},
	}
	for _, c := range cases {
		m, s := Summarize(c.vals, c.w, c.sig, c.consensus, cfg)
		if math.Abs(m-c.wantMean) > 1e-9 || math.Abs(s-c.wantS) > 1e-9 {
			t.Errorf("%s: got (%.3f, %.3f) want (%.3f, %.3f)", c.name, m, s, c.wantMean, c.wantS)
		}
	}
}

func TestParseADP(t *testing.T) {
	csv := "Consensus,Player,Pos,Team,Bye,A,B,C,D,E,Ignored\n" +
		"1,Jahmyr Gibbs,RB1,DET,6,1,1,2,1,1,99\n" +
		"50,Some Kicker,PK1,DAL,7,50,50,50,50,50,1\n" +
		"166,Travis Hunter,CB1,JAX,7,160,170,-,180,-,1\n" +
		"20,Team Defense,TD1,BAL,7,100,-,-,-,-,1\n" +
		"30,Tight End,TE2,KCC,10,30,30,30,-,10,1\n"
	list, err := ParseADP(strings.NewReader(csv), testCfg(), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]*players.Player{}
	for _, p := range list {
		byID[p.ID] = p
	}
	if len(list) != 4 {
		t.Fatalf("want 4 players (kicker dropped), got %d: %v", len(list), list)
	}
	if h := byID["travis-hunter-jax"]; h == nil || h.Pos != players.WR {
		t.Errorf("Hunter should be WR: %+v", h)
	}
	if d := byID["team-defense-bal"]; d == nil || d.Pos != players.DST || d.ADPStdDev != 15 {
		t.Errorf("thin DST: %+v", d)
	}
	if g := byID["jahmyr-gibbs-det"]; g == nil || g.ADPMean != 1 || g.ADPStdDev != 3 || g.Team != "DET" || len(g.ADPSources) != 5 {
		t.Errorf("gibbs: %+v", g)
	}
	// TE: E's 10 carries weight 0.3 so the median stays at 30.
	if te := byID["tight-end-kc"]; te == nil || te.ADPMean != 30 {
		t.Errorf("te weight override: %+v", te)
	}
	if list[0].ID != "jahmyr-gibbs-det" {
		t.Errorf("not sorted by ADP: %s", list[0].ID)
	}
}
