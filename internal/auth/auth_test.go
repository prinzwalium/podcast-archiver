package auth

import "testing"

func TestHasGroup(t *testing.T) {
	allowed := map[string]bool{"podcast-users": true}
	admin := map[string]bool{"podcast-admins": true}

	cases := []struct {
		name   string
		groups []string
		want   bool
	}{
		{"member", []string{"podcast-users"}, true},
		{"member among others", []string{"other", "podcast-users"}, true},
		{"not a member", []string{"other-group"}, false},
		{"no groups", nil, false},
	}
	for _, tc := range cases {
		if got := hasGroup(tc.groups, allowed); got != tc.want {
			t.Errorf("%s: hasGroup = %v, want %v", tc.name, got, tc.want)
		}
		if got := hasGroup(tc.groups, admin); got != false && tc.name == "not a member" {
			t.Errorf("%s: unexpectedly admin", tc.name)
		}
	}

	// Empty want-set must never grant access.
	if hasGroup([]string{"anything"}, map[string]bool{}) {
		t.Error("empty want-set should not match")
	}
}

func TestToggleTrailingSlash(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://auth.example.com/application/o/app", "https://auth.example.com/application/o/app/"},
		{"https://auth.example.com/application/o/app/", "https://auth.example.com/application/o/app"},
	}
	for _, tc := range cases {
		if got := toggleTrailingSlash(tc.in); got != tc.want {
			t.Errorf("toggleTrailingSlash(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
