package players

import "testing"

func TestNames(t *testing.T) {
	cases := []struct{ in, norm, key, slug string }{
		{"Chase, Ja'Marr", "Ja'Marr Chase", "jamarr chase", "jamarr-chase-cin"},
		{"St. Brown, Amon-Ra", "Amon-Ra St. Brown", "amon ra st brown", "amon-ra-st-brown-det"},
		{"Walker III, Kenneth", "Kenneth Walker", "kenneth walker", "kenneth-walker-kc"},
		{"Ken Walker III", "Ken Walker", "kenneth walker", "kenneth-walker-kc"},
		{"Marvin Harrison Jr.", "Marvin Harrison", "marvin harrison", "marvin-harrison-ari"},
	}
	teams := []string{"CIN", "DET", "KCC", "KC", "ARI"}
	for i, c := range cases {
		if got := NormalizeName(c.in); got != c.norm {
			t.Errorf("NormalizeName(%q)=%q want %q", c.in, got, c.norm)
		}
		if got := NameKey(c.in); got != c.key {
			t.Errorf("NameKey(%q)=%q want %q", c.in, got, c.key)
		}
		if got := Slug(c.in, teams[i]); got != c.slug {
			t.Errorf("Slug(%q)=%q want %q", c.in, got, c.slug)
		}
	}
	if NormPos("D/ST") != DST || NormPos("PK") != K || K.Draftable() || !DST.Draftable() {
		t.Error("position normalisation")
	}
}
