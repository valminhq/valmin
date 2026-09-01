// Package resolver computes a mod install's dependency closure — 03 §6.3's diamond
// resolution and cycle detection — as pure functions over caller-supplied lookups. It
// imports neither store nor api (CLAUDE.md §5): the caller (WP-07's job runner) already
// holds the mod_versions rows and the instance's installed set, and passes them in rather
// than this package reading either directly.
package resolver
