package strategy

import "testing"

func TestLoadReal(t *testing.T) {
	c, err := Load("../../data/strategy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if c.Engine.Sims != 2000 || c.ADP.Sources["FFPC"].WeightFor("TE") != 0.3 || c.ADP.Sources["FFPC"].WeightFor("RB") != 1.0 {
		t.Errorf("unexpected config: %+v", c.Engine)
	}
	if len(c.Gates) != 4 || c.LeagueRoster().TotalSpots != 17 {
		t.Errorf("gates/roster: %+v", c.Gates)
	}
}
