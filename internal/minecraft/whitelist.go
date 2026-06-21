package minecraft

import (
	"sort"
	"strings"
)

// Whitelist sync over RCON. The control plane owns a server's whitelist: it is
// exactly the set of player usernames the owner granted onto that server. The
// guest reads the live whitelist with "whitelist list", diffs it against the
// desired set, and applies the minimal add/remove plus an on/off toggle.

// ParseWhitelist extracts the usernames from a vanilla "/whitelist list" reply,
// e.g. "There are 2 whitelisted players: alice, bob" -> [alice bob]. A reply
// with no list ("There are no whitelisted players") yields an empty slice. It is
// tolerant of section-sign color codes and a trailing period.
func ParseWhitelist(body string) []string {
	body = colorCodes.ReplaceAllString(body, "")
	idx := strings.IndexByte(body, ':')
	if idx < 0 {
		return nil
	}
	var out []string
	for _, part := range strings.Split(body[idx+1:], ",") {
		name := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(part), "."))
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

// WhitelistSyncCommands returns the RCON commands that reconcile a server's live
// whitelist (current) to the desired set: a "whitelist add" for each newly
// granted name and a "whitelist remove" for each revoked one (matched
// case-insensitively, since Minecraft whitelist lookups ignore case, so only a
// real membership change produces a command). It then enables enforcement when
// the desired set is non-empty, or disables it when empty — so a server with no
// granted players is left open rather than locking everyone out. Returns the
// commands in a stable order (adds, then removes, then the toggle) for
// determinism. When nothing changes it still returns the single on/off toggle,
// which is idempotent.
func WhitelistSyncCommands(current, desired []string) []string {
	curByLower := lowerIndex(current)
	desByLower := lowerIndex(desired)

	var adds, removes []string
	for lower, orig := range desByLower {
		if _, ok := curByLower[lower]; !ok {
			adds = append(adds, orig)
		}
	}
	for lower, orig := range curByLower {
		if _, ok := desByLower[lower]; !ok {
			removes = append(removes, orig)
		}
	}
	sort.Strings(adds)
	sort.Strings(removes)

	cmds := make([]string, 0, len(adds)+len(removes)+1)
	for _, a := range adds {
		cmds = append(cmds, "whitelist add "+a)
	}
	for _, rm := range removes {
		cmds = append(cmds, "whitelist remove "+rm)
	}
	if len(desByLower) == 0 {
		cmds = append(cmds, "whitelist off")
	} else {
		cmds = append(cmds, "whitelist on")
	}
	return cmds
}

// lowerIndex maps each name's lowercase form to its original spelling, collapsing
// case-duplicates (the last spelling wins) the way the server's own matching does.
func lowerIndex(names []string) map[string]string {
	m := make(map[string]string, len(names))
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			m[strings.ToLower(n)] = n
		}
	}
	return m
}
