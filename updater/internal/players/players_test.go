package players

import "testing"

func TestDecision(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b Observation
		want State
	}{
		{"empty", Observation{0, true}, Observation{0, true}, Zero},
		{"occupied", Observation{2, true}, Observation{2, true}, NonZero},
		{"timeout-empty", Observation{}, Observation{0, true}, Unknown},
		{"timeout-occupied", Observation{}, Observation{2, true}, NonZero},
		{"disagreement", Observation{0, true}, Observation{2, true}, Unknown},
		{"invalid", Observation{}, Observation{}, Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Decide(tc.a, tc.b); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}

func TestParsers(t *testing.T) {
	for _, tc := range []struct {
		text  string
		count int
		valid bool
	}{
		{"There are 0 of a max of 20 players online:", 0, true},
		{"There are 2 of a max of 20 players online: abc, def", 2, true},
		{"There are 2 of a max of 20 players online: abc", 0, false},
		{"garbage", 0, false},
	} {
		if got := RCON(tc.text); got != (Observation{tc.count, tc.valid}) {
			t.Errorf("%q: %+v", tc.text, got)
		}
	}
	if got := Monitor("127.0.0.1:25565 : version=1.21.1 online=0 max=20 motd='hello'"); got != (Observation{0, true}) {
		t.Fatal(got)
	}
}
