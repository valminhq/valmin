// Package remotecopy runs the jobs that copy backup archives to off-host storage, retry them,
// and remove remote copies that retention no longer keeps.
package remotecopy

import (
	"context"
	"errors"

	"github.com/valminhq/valmin/internal/backup/remote"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

const (
	LockKey     = "global:remote_backup"
	CopyIDField = "copy_id"
)

// Worker dispatches uploads and retention cleanup from durable remote-copy records.
type Worker struct {
	DB         *store.DB
	Engine     *jobs.Engine
	BackendFor func(*store.RemoteDestination) (remote.Backend, error)
}

// safeError hides destination details from job records.
func safeError(err error) string {
	var failure *remote.Failure
	if errors.As(err, &failure) {
		return failure.Message
	}
	if errors.Is(err, remote.ErrConfiguration) {
		return "Remote configuration or address is not allowed."
	}
	if errors.Is(err, remote.ErrNotFound) {
		return "Remote object was not found."
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "Remote transfer exceeded its deadline."
	}
	return "Remote copy could not be completed."
}

// Failed maps a remote failure to a job outcome.
func Failed(message string) jobs.Outcome {
	return jobs.Outcome{Status: jobs.StatusFailed, ErrorCode: errcode.Unavailable.String(), Error: message}
}
