package model

import (
	"regexp"
	"time"
)

// usernamePattern matches a Minecraft-style player name: 3–16 characters of
// letters, digits, or underscore. The whitelist roster validates against it so a
// stored name is always a plausible account name.
var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9_]{3,16}$`)

// ValidUsername reports whether s is a well-formed player username.
func ValidUsername(s string) bool {
	return usernamePattern.MatchString(s)
}

// Player is one entry in a user's whitelist roster: a username plus the set of
// the user's servers that player is allowed to use. ServerIDs holds only live
// (non-deleted) servers the owner still has; a player granted on no server is a
// roster entry with an empty ServerIDs.
type Player struct {
	ID        string    `json:"id"`
	OwnerID   string    `json:"owner_id"`
	Username  string    `json:"username"`
	ServerIDs []string  `json:"server_ids"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
