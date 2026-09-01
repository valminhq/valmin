// Package installer decides where a staged Thunderstore package's files belong, what
// applying them would change, and what the file manifest that makes uninstall exact
// records (03 §6.4, ADR-009). It imports neither store nor api and writes nothing: it is
// a pure function over a filesystem (CLAUDE.md §5), so WP-07's job runner owns every
// write and this package owns every decision.
//
// Specification: 03 §6.4, 03 §6.5, 02 §4.2, 04 §2, ADR-009, ADR-106.
package installer
