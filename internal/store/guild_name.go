package store

import "strings"

// PlaceholderGuildNamePrefix prefixes the stand-in guild display name used when
// no real guild name is available from Discord. Writers that mint the stand-in
// build it from this constant so the store can recognize it again.
const PlaceholderGuildNamePrefix = "Discord Desktop Guild "

// Guild name ranks order names by how much they say about a guild. guilds.name
// only ever moves toward a higher rank.
const (
	guildNameRankBlank    = 0
	guildNameRankStandIn  = 1
	guildNameRankResolved = 2
)

// isPlaceholderGuildName reports whether name is a stand-in that says no more
// about the guild than its id already does: the synthetic
// "Discord Desktop Guild <id>" form, or the bare guild id. A name that merely
// looks numeric is a real name, because a guild may be named with digits.
func isPlaceholderGuildName(id, name string) bool {
	if strings.HasPrefix(name, PlaceholderGuildNamePrefix) {
		return true
	}
	trimmed := strings.TrimSpace(name)
	return trimmed != "" && trimmed == strings.TrimSpace(id)
}

func guildNameRank(id, name string) int {
	if strings.TrimSpace(name) == "" {
		return guildNameRankBlank
	}
	if isPlaceholderGuildName(id, name) {
		return guildNameRankStandIn
	}
	return guildNameRankResolved
}

// ResolveGuildName returns the name a guild upsert should store. A guild name
// only ever moves toward more information, never less: an incoming name that
// says less about the guild than the stored name is discarded, and every other
// incoming name is written, so a genuine rename still applies. Pass an empty
// stored name for a guild the archive has not seen.
func ResolveGuildName(id, incoming, stored string) string {
	if guildNameRank(id, incoming) >= guildNameRank(id, stored) {
		return incoming
	}
	return stored
}
