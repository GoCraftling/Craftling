package minecraft

import (
	"reflect"
	"testing"
)

func TestParseWhitelist(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"two players", "There are 2 whitelisted players: alice, bob", []string{"alice", "bob"}},
		{"one player", "There are 1 whitelisted players: Steve", []string{"Steve"}},
		{"none", "There are no whitelisted players", nil},
		{"trailing period", "Whitelisted players: alice, bob.", []string{"alice", "bob"}},
		{"color codes", "§aThere are 1 whitelisted players: §balice", []string{"alice"}},
		{"no colon", "garbage output", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseWhitelist(tc.body); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseWhitelist(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

func TestWhitelistSyncCommands(t *testing.T) {
	cases := []struct {
		name             string
		current, desired []string
		want             []string
	}{
		{
			name:    "add and remove against a non-empty set",
			current: []string{"bob", "carol"},
			desired: []string{"alice", "bob"},
			want:    []string{"whitelist add alice", "whitelist remove carol", "whitelist on"},
		},
		{
			name:    "no membership change still toggles on",
			current: []string{"alice", "bob"},
			desired: []string{"bob", "alice"},
			want:    []string{"whitelist on"},
		},
		{
			name:    "case-insensitive match produces no churn",
			current: []string{"Alice"},
			desired: []string{"alice"},
			want:    []string{"whitelist on"},
		},
		{
			name:    "empty desired clears and disables",
			current: []string{"alice", "bob"},
			desired: nil,
			want:    []string{"whitelist remove alice", "whitelist remove bob", "whitelist off"},
		},
		{
			name:    "empty both leaves it off",
			current: nil,
			desired: nil,
			want:    []string{"whitelist off"},
		},
		{
			name:    "fresh enable from empty",
			current: nil,
			desired: []string{"alice"},
			want:    []string{"whitelist add alice", "whitelist on"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := WhitelistSyncCommands(tc.current, tc.desired); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("WhitelistSyncCommands(%v, %v) = %v, want %v", tc.current, tc.desired, got, tc.want)
			}
		})
	}
}
