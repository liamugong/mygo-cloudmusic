package netease

import "testing"

// A stream response's level is backfilled with the requested level when the
// server omits one, so the raw server value and whether it was a known level
// are recorded before the backfill: a client-side guess must not read as a
// served level.
func TestLevelProvenance(t *testing.T) {
	cases := []struct {
		served   string
		wantRaw  string
		reported bool
	}{
		{"sky", "sky", true},
		{"lossless", "lossless", true},
		{"", "", false},
		{"unknown", "unknown", false},
	}
	for _, c := range cases {
		raw, reported := levelProvenance(c.served)
		if raw != c.wantRaw || reported != c.reported {
			t.Errorf("levelProvenance(%q) = %q, %v; want %q, %v", c.served, raw, reported, c.wantRaw, c.reported)
		}
	}
}

func TestPlayableFollowsPrivilege(t *testing.T) {
	tracks := []Track{{ID: 1, Fee: 1}, {ID: 2, Fee: 8}, {ID: 3, Fee: 8}, {ID: 4, Fee: 1}}
	if tracks[0].Playable() || !tracks[1].Playable() {
		t.Fatal("without privileges, VIP songs do not play and free ones do")
	}
	withPrivileges(tracks, []Privilege{
		{ID: 1, Pl: 320000},           // VIP song, the listener subscribes
		{ID: 2, Pl: 0},                // nothing to play
		{ID: 3, St: -200, Pl: 128000}, // taken down
	})
	want := []bool{true, false, false, false}
	for i, tr := range tracks {
		if tr.Playable() != want[i] {
			t.Errorf("track %d: playable %v", tr.ID, tr.Playable())
		}
	}
}
