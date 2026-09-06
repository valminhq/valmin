package instance

import (
	"context"
	"fmt"
	"strings"

	"github.com/valminhq/valmin/internal/runtime"
)

// QueryPublicBuild reads Steam metadata in an isolated container with no instance mounts.
func QueryPublicBuild(ctx context.Context, rt runtime.Runtime, image string) (string, error) {
	var output steamOutput
	code, err := runtime.RunThrowaway(ctx, rt, &runtime.ThrowawaySpec{
		Image: image, User: containerUser, Env: []string{"HOME=/tmp"},
		Cmd:    []string{"+login", "anonymous", "+app_info_print", AppID, "+quit"},
		Stdout: &output, Stderr: &output,
	})
	if err != nil {
		return "", fmt.Errorf("query Steam metadata: %w", err)
	}
	if code != 0 {
		return "", fmt.Errorf("steam metadata query exited %d", code)
	}
	return PublicBuildID(output.String())
}

type steamOutput struct{ strings.Builder }

func (b *steamOutput) Write(p []byte) (int, error) {
	if len(p) > steamMetadataLimit-b.Len() {
		return 0, fmt.Errorf("steam output exceeds %d bytes", steamMetadataLimit)
	}
	n, err := b.Builder.Write(p)
	if err != nil {
		return n, fmt.Errorf("capture steam output: %w", err)
	}
	return n, nil
}
