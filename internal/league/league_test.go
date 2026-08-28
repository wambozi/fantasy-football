package league

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/wambozi/draft-copilot/internal/players"
)

func TestParseRoundPick(t *testing.T) {
	cases := []struct {
		in      string
		r, p    int
		wantErr bool
	}{
		{"1.1", 1, 1, false},
		{"1.10", 1, 10, false}, // must not collapse to 1.1
		{"17.12", 17, 12, false},
		{" 3.4 ", 3, 4, false},
		{"1", 0, 0, true},
		{"0.5", 0, 0, true},
		{"a.b", 0, 0, true},
	}
	for _, c := range cases {
		r, p, err := parseRoundPick(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("%q: err=%v wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if r != c.r || p != c.p {
			t.Errorf("%q: got %d.%d want %d.%d", c.in, r, p, c.r, c.p)
		}
	}
}

func TestLivePicksRealCSV(t *testing.T) {
	lg, err := Load("../../data/draft-order.csv", "Sittin Purdy", DefaultRoster())
	if err != nil {
		t.Fatal(err)
	}
	want := []int{8, 11, 26, 49, 65, 68, 85, 90}
	if got := lg.MyLivePicks[:8]; !reflect.DeepEqual(got, want) {
		t.Fatalf("MyLivePicks[0..7] = %v, want %v", got, want)
	}
	if len(lg.MyLivePicks) != 15 {
		t.Errorf("want 15 live picks, got %d", len(lg.MyLivePicks))
	}
	if len(lg.Teams) != 12 || lg.NumLive() != 204-21 {
		t.Errorf("teams=%d live=%d", len(lg.Teams), lg.NumLive())
	}
	if got := lg.TeamOnClock(1); got != "Patient Zeros Aids Epidemic" {
		t.Errorf("live 1 on clock = %q", got)
	}
	if got := lg.NextLivePickFor("Sittin Purdy", 9); got != 11 {
		t.Errorf("next for me from 9 = %d", got)
	}
}

func TestParseKeeperFields(t *testing.T) {
	csv := "Round/Pick,Team,Amount,Player,\"Player Team\",\"Player Position\"\n" +
		"1.1,A,0,,,\n" +
		"1.2,B,0,\"Gibbs, Jahmyr\",DET,RB\n"
	slots, err := ParseDraftOrder(strings.NewReader(csv))
	if err != nil {
		t.Fatal(err)
	}
	if slots[0].KeeperID != "" || slots[1].KeeperID != "jahmyr-gibbs-det" {
		t.Errorf("keeper ids: %q %q", slots[0].KeeperID, slots[1].KeeperID)
	}
	if slots[1].KeeperPos != players.RB || slots[1].KeeperName != "Jahmyr Gibbs" {
		t.Errorf("keeper fields: %+v", slots[1])
	}
}

func TestValidateRejectsUnevenOwnership(t *testing.T) {
	slots := []DraftSlot{{Round: 1, PickInRound: 1, Team: "A"}, {Round: 1, PickInRound: 2, Team: "B"}}
	r := DefaultRoster()
	r.TotalSpots = 2
	if _, err := New(slots, "A", r); err == nil {
		t.Error("expected error for 1 slot per team with 2 spots")
	}
}

func TestMain(m *testing.M) {
	if _, err := os.Stat("../../data/draft-order.csv"); err != nil {
		panic("data/draft-order.csv missing")
	}
	os.Exit(m.Run())
}
