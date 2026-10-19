// Package remote implements storage operations for off-host backup copies.
package remote

import (
	"context"
	"errors"
	"path"
	"strings"
)

// Backend stores, reads and deletes the objects of one off-host destination.
type Backend interface {
	// Check confirms the destination is configured well enough to try a transfer.
	Check(ctx context.Context) error
	// Put uploads the file at localPath under key.
	Put(ctx context.Context, key, localPath string) (Object, error)
	// Stat reads an uploaded object's size and provider id, or returns ErrNotFound.
	Stat(ctx context.Context, ref ObjectRef) (Object, error)
	// Delete removes an object; one already gone is not an error.
	Delete(ctx context.Context, ref ObjectRef) error
}

// ObjectRef names an uploaded object: its key under the destination, and the id the provider
// gave it where it has one.
type ObjectRef struct {
	Key        string `json:"key"`
	ProviderID string `json:"provider_id,omitempty"`
}

// Object is an uploaded object and its size.
type Object struct {
	Ref       ObjectRef `json:"ref"`
	SizeBytes int64     `json:"size_bytes"`
}

var (
	// ErrNotFound reports an object the destination does not hold.
	ErrNotFound = errors.New("remote object not found")
	// ErrConfiguration reports a destination whose settings cannot work.
	ErrConfiguration = errors.New("remote destination configuration is invalid")
)

// Failure is a transfer that failed, with a message safe to show and whether trying again may
// work.
type Failure struct {
	Message   string
	Temporary bool
}

func (e *Failure) Error() string { return e.Message }

// Retryable reports whether err is a Failure that trying again may fix.
func Retryable(err error) bool {
	var failure *Failure
	return errors.As(err, &failure) && failure.Temporary
}

// ValidKey excludes traversal and rclone's inline backend syntax.
func ValidKey(key string) bool {
	return key != "" && key != "." && !strings.HasPrefix(key, "/") &&
		path.Clean(key) == key && key != ".." && !strings.HasPrefix(key, "../") &&
		!strings.ContainsAny(key, "\\:\x00\r\n")
}
