package canvas

import "hash/fnv"

// icons is a curated set of visually distinct emoji glyphs used as a
// canvas's default icon when none is given explicitly via `scrim add
// --icon`. Picking from a fixed set (rather than, say, the first rune of
// the title) keeps the default glyph legible at gallery-card size and
// doesn't depend on any particular font's coverage.
var icons = []string{
	"🎨", "🚀", "🔬", "📊", "🧩", "🛠️", "📡", "🌈", "🔮", "🧭",
	"🪐", "🐙", "🦉", "🐝", "🦄", "🍄", "🌵", "🌊", "🔥", "❄️",
	"⚡", "🎯", "🧪", "🗺️", "🎲", "🧵", "🪄", "🔑", "📎", "🧱",
}

// colors is a curated set of accent colors, hashed onto a canvas
// independently of its icon (see DefaultColor) so the glyph+color pairing
// varies rather than every icon always landing on the same color.
var colors = []string{
	"#ef4444", "#f97316", "#f59e0b", "#84cc16", "#22c55e",
	"#10b981", "#14b8a6", "#06b6d4", "#3b82f6", "#6366f1",
	"#8b5cf6", "#a855f7", "#d946ef", "#ec4899", "#f43f5e",
}

// DefaultIcon deterministically derives an emoji glyph from id: the same id
// always picks the same glyph (stable across daemon restarts, since it's a
// pure function of id rather than stored state), and different ids are
// spread across the curated set.
func DefaultIcon(id string) string {
	return icons[hashIndex(id, "icon", len(icons))]
}

// DefaultColor deterministically derives an accent color from id, the same
// way DefaultIcon does -- hashed with a distinct salt so a given id's glyph
// and color don't move in lockstep (e.g. every 3rd id landing on the same
// color as every 3rd icon).
func DefaultColor(id string) string {
	return colors[hashIndex(id, "color", len(colors))]
}

// hashIndex hashes salt+id with FNV-1a (stdlib, non-cryptographic -- this
// only needs to be a stable, well-distributed pick from a small fixed set,
// not collision-resistant) and reduces it into [0, n).
func hashIndex(id, salt string, n int) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(salt + ":" + id)) // hash.Hash.Write never errors
	return int(h.Sum32() % uint32(n))       //nolint:gosec // n is always a small curated-slice length, well within int range
}
