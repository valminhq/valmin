package backup

import (
	"fmt"
	"time"
)

// Shrunk reports whether a world of size bytes is under half of baseline. Worlds shrink a little
// between saves as items despawn and terrain is optimised; a halving is what a truncated save or
// a world replaced underneath the panel looks like.
func Shrunk(bytes, baseline int64) bool {
	return baseline > 0 && 2*bytes < baseline
}

// ShrunkAttention is the finding for a world that shrank against the last archive that passed,
// taken at baselineAt.
func ShrunkAttention(bytes, baseline int64, baselineAt time.Time) Attention {
	return Attention{AttentionWorldShrunk, fmt.Sprintf(
		"the world is %s, under half of the %s in the archive of %s",
		formatBytes(bytes), formatBytes(baseline), baselineAt.UTC().Format("2006-01-02 15:04 UTC"))}
}
