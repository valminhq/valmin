// Package cache is the content-addressed Thunderstore zip cache (03 §6.1, 03 §6.2,
// 02 §3): a package version is downloaded at most once per host and shared across every
// instance that installs it. It imports neither store nor api (CLAUDE.md §5) — the
// caller supplies the download URL and the declared size, both already known to whoever
// resolved the closure.
package cache
