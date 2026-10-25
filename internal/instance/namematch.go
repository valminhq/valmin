package instance

import (
	"slices"
	"time"
)

// matchWindow is how long a connecting account stays a candidate for the next character to
// spawn. The spawn line followed the socket line by 16 to 21 seconds in the captures; a
// heavily modded server loads slower, so widen it if names go unmatched there.
const matchWindow = 2 * time.Minute

// maxSpawned bounds the connection numbers a matcher remembers.
const maxSpawned = 256

// candidate is an account whose socket connected and whose character has not spawned yet.
type candidate struct {
	remoteID   string
	platformID string
	at         time.Time
}

// nameMatcher ties character names to the accounts that were waiting to spawn when the name
// appeared. The spawn line names no account, so every waiting account is credited with the
// name, and the store counts how often each pairing happens: the real owner collects a hit
// every session, a bystander only when the two joined together.
//
// It is owned by the log reader's run goroutine and is not safe for concurrent use.
type nameMatcher struct {
	waiting []candidate
	// spawnedAt are the times characters appeared since the oldest waiting account connected.
	spawnedAt []time.Time
	// spawned holds the connection numbers already seen, so a respawn after a death, which
	// keeps its number, is not counted as a new player. seenOrder lists them oldest first.
	spawned   map[string]struct{}
	seenOrder []string
}

// connected adds an account whose socket presented its platform id. A reconnect replaces the
// account's earlier entry, so it is credited once per spawn.
func (m *nameMatcher) connected(remoteID, platformID string, at time.Time) {
	m.drop(func(c candidate) bool { return c.remoteID == remoteID || c.platformID == platformID })
	m.waiting = append(m.waiting, candidate{remoteID: remoteID, platformID: platformID, at: at})
}

// refused drops a socket the server turned away: it never spawns a character.
func (m *nameMatcher) refused(remoteID string) {
	m.drop(func(c candidate) bool { return c.remoteID == remoteID })
	m.settle()
}

// settle empties the list once as many characters appeared as there are accounts waiting,
// which is also true after the account that never spawned left it.
func (m *nameMatcher) settle() {
	if len(m.waiting) > 0 && m.spawnsSinceOldest() >= len(m.waiting) {
		m.waiting, m.spawnedAt = m.waiting[:0], m.spawnedAt[:0]
	}
}

// drop removes the waiting accounts for which gone reports true.
func (m *nameMatcher) drop(gone func(candidate) bool) {
	kept := m.waiting[:0]
	for _, c := range m.waiting {
		if !gone(c) {
			kept = append(kept, c)
		}
	}
	m.waiting = kept
}

// spawn reads a character line and returns one guessed identity for each account that could
// own the name. A destroyed character (connection number 0) and a respawn return nothing.
func (m *nameMatcher) spawn(name, number string, at time.Time) []PlayerIdentity {
	if number == "0" {
		return nil
	}
	if _, again := m.spawned[number]; again {
		return nil
	}
	m.remember(number)

	m.dropStale(at)
	m.settle()
	if len(m.waiting) == 0 {
		return nil
	}
	out := make([]PlayerIdentity, 0, len(m.waiting))
	for _, c := range m.waiting {
		out = append(out, PlayerIdentity{TS: at, PlatformID: c.platformID, Name: name, Guessed: true})
	}
	m.spawnedAt = append(m.spawnedAt, at)
	m.settle()
	return out
}

// remember records number as seen, forgetting the oldest one once maxSpawned are held.
func (m *nameMatcher) remember(number string) {
	if m.spawned == nil {
		m.spawned = make(map[string]struct{}, maxSpawned)
	}
	if len(m.seenOrder) >= maxSpawned {
		delete(m.spawned, m.seenOrder[0])
		m.seenOrder = m.seenOrder[1:]
	}
	m.spawned[number] = struct{}{}
	m.seenOrder = append(m.seenOrder, number)
}

// spawnsSinceOldest counts the characters that appeared since the oldest waiting account
// connected, forgetting the ones before it. Once that many characters appeared as there are
// accounts waiting, every one of them has spawned.
func (m *nameMatcher) spawnsSinceOldest() int {
	oldest := m.waiting[0].at
	for _, c := range m.waiting {
		if c.at.Before(oldest) {
			oldest = c.at
		}
	}
	m.spawnedAt = slices.DeleteFunc(m.spawnedAt, func(t time.Time) bool { return t.Before(oldest) })
	return len(m.spawnedAt)
}

// dropStale forgets accounts that connected longer ago than matchWindow.
func (m *nameMatcher) dropStale(now time.Time) {
	m.drop(func(c candidate) bool { return now.Sub(c.at) > matchWindow })
	if len(m.waiting) == 0 {
		m.spawnedAt = m.spawnedAt[:0]
	}
}
