package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/wambozi/draft-copilot/internal/players"
)

func TestApplyProjectionsAndECR(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "projections"), 0o755)
	// FantasyPros-style per-position file with duplicated stat headers and a quoted name.
	os.WriteFile(filepath.Join(dir, "projections", "FantasyPros_2026_Projections_RB.csv"), []byte(
		"\"Player\",\"Team\",\"ATT\",\"YDS\",\"TDS\",\"REC\",\"YDS\",\"TDS\",\"FL\",\"FPTS\"\n"+
			"\"Bijan Robinson\",\"ATL\",\"300\",\"1,400\",\"12\",\"60\",\"500\",\"3\",\"2\",\"306.0\"\n"+
			"\"Nobody Here\",\"FA\",\"1\",\"1\",\"0\",\"0\",\"0\",\"0\",\"0\",\"5\"\n"), 0o644)
	// Combined file with a POS column.
	os.WriteFile(filepath.Join(dir, "projections", "all.csv"), []byte(
		"Player,Pos,Points\nJosh Allen,QB1,398.2\nTravis Hunter,WR,180\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "ecr.csv"), []byte(
		"\"RK\",\"TIERS\",\"PLAYER NAME\",\"TEAM\",\"POS\",\"BEST\",\"WORST\",\"AVG\",\"STD.DEV\"\n"+
			"\"1\",\"1\",\"Bijan Robinson\",\"ATL\",\"RB1\",\"1\",\"3\",\"1.4\",\"0.6\"\n"+
			"\"9\",\"2\",\"Josh Allen\",\"BUF\",\"QB1\",\"5\",\"20\",\"11.2\",\"3.1\"\n"), 0o644)

	list := []*players.Player{
		{ID: "bijan", Name: "Bijan Robinson", Pos: players.RB},
		{ID: "allen", Name: "Josh Allen", Pos: players.QB},
		{ID: "hunter-wr", Name: "Travis Hunter", Pos: players.WR},
		{ID: "hunter-cb", Name: "Travis Hunter", Pos: players.DST}, // ambiguous by name alone
	}
	n, err := applyProjections(dir, list, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("matched %d, want 3", n)
	}
	if list[0].ProjPoints != 336 || list[0].ECR != 1.4 {
		t.Errorf("bijan: %+v", list[0])
	}
	if list[1].ProjPoints != 398.2 || list[1].ECR != 11.2 {
		t.Errorf("allen: %+v", list[1])
	}
	if list[2].ProjPoints != 180 || list[3].ProjPoints != 0 {
		t.Errorf("hunter: wr=%v dst=%v", list[2].ProjPoints, list[3].ProjPoints)
	}
}
