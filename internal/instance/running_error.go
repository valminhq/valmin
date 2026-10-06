package instance

import "errors"

// ErrServerRunning means a container is active while the instance row says stopped.
var ErrServerRunning = errors.New("the server is running although this instance is recorded as stopped")
