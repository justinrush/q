package main

import (
	"maps"
	"testing"
)

// --base names a repo rather than a path, because a mission's base branches are
// keyed by name, which is also how the board and the operation refer to a repo.
func TestParseBaseFlags(t *testing.T) {
	cases := []struct {
		name    string
		values  []string
		want    map[string]string
		wantErr bool
	}{
		{
			name: "no flags means no overrides",
		},
		{
			name:   "one repo on a branch",
			values: []string{"q=feat/x"},
			want:   map[string]string{"q": "feat/x"},
		},
		{
			name:   "repeated flags accumulate",
			values: []string{"q=feat/x", "mac=main"},
			want:   map[string]string{"q": "feat/x", "mac": "main"},
		},
		{
			name:   "a branch containing an equals sign survives",
			values: []string{"q=feat/a=b"},
			want:   map[string]string{"q": "feat/a=b"},
		},
		{
			name:   "surrounding space is trimmed",
			values: []string{" q = feat/x "},
			want:   map[string]string{"q": "feat/x"},
		},
		{
			name:    "a value with no branch is refused",
			values:  []string{"q"},
			wantErr: true,
		},
		{
			name:    "an empty branch is refused",
			values:  []string{"q="},
			wantErr: true,
		},
		{
			name:    "an empty repo is refused",
			values:  []string{"=main"},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseBaseFlags(tc.values)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseBaseFlags(%q) succeeded, want an error", tc.values)
				}

				return
			}

			if err != nil {
				t.Fatalf("parseBaseFlags(%q): %v", tc.values, err)
			}

			if !maps.Equal(got, tc.want) {
				t.Errorf("parseBaseFlags(%q) = %v, want %v", tc.values, got, tc.want)
			}
		})
	}
}
