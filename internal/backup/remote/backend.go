// Package remote implements storage operations for off-host backup copies.
package remote

import (
	"context"
	"errors"
	"path"
	"strings"
)

type Backend interface {
	Put(ctx context.Context, key, localPath string) (Object, error)
	Stat(ctx context.Context, ref ObjectRef) (Object, error)
	Delete(ctx context.Context, ref ObjectRef) error
}

type ObjectRef struct {
	Key        string `json:"key"`
	ProviderID string `json:"provider_id,omitempty"`
}
type Object struct {
	Ref       ObjectRef `json:"ref"`
	SizeBytes int64     `json:"size_bytes"`
}

var (
	ErrNotFound      = errors.New("remote object not found")
	ErrConfiguration = errors.New("remote destination configuration is invalid")
)

type Failure struct {
	Message   string
	Temporary bool
}

func (e *Failure) Error() string { return e.Message }
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
