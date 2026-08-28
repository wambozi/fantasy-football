package players

import "testing"

func TestSearch(t *testing.T) {
	p := &Pool{ByID: map[string]*Player{}}
	p.Add(&Player{ID: "jsn", Name: "Jaxon Smith-Njigba", Pos: WR, Team: "SEA", ADPMean: 10})
	p.Add(&Player{ID: "jud", Name: "Quinshon Judkins", Pos: RB, Team: "CLE", ADPMean: 40})
	p.Add(&Player{ID: "jj", Name: "Justin Jefferson", Pos: WR, Team: "MIN", ADPMean: 5})
	p.Add(&Player{ID: "k", Name: "Harrison Butker", Pos: K, Team: "KC", ADPMean: 150})
	cases := map[string]string{"jud": "jud", "jsn": "jsn", "smith": "jsn", "jeff": "jj", "j": "jj"}
	for q, want := range cases {
		got := p.Search(q, nil, 5)
		if len(got) == 0 || got[0].ID != want {
			t.Errorf("Search(%q) first = %v, want %s", q, got, want)
		}
	}
	if got := p.Search("butk", nil, 5); len(got) != 0 {
		t.Errorf("kicker should not be searchable, got %v", got)
	}
	ex := func(pl *Player) bool { return pl.ID == "jj" }
	if got := p.Search("j", ex, 5); got[0].ID == "jj" {
		t.Errorf("exclude ignored")
	}
}
