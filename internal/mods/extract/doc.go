// Package extract unpacks a third-party Thunderstore zip into a staging directory with no
// path, mode or size outcome that the archive itself chooses (B5, 03 §6.5). It imports
// neither store nor api, so it is a pure function over a filesystem (CLAUDE.md §5).
//
// Specification: 03 §6.5, 03 §6.3.
package extract
