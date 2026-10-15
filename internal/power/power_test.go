package power

import "testing"

func TestFullyAwake(t *testing.T) {
	cases := map[string]struct {
		in   string
		want bool
	}{
		"full wake": {
			"Current System Capabilities are: CPU Graphics Audio Network \nCurrent Power State: 4\n",
			true,
		},
		"dark wake": {
			"Current System Capabilities are: CPU Network \nCurrent Power State: 4\n",
			false,
		},
		"no capabilities": {"Current System Capabilities are:\n", false},
		"unrecognized":    {"pmset: something else entirely\n", true},
		"empty":           {"", true},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := FullyAwake(tc.in); got != tc.want {
				t.Errorf("FullyAwake(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
