package instance

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/valminhq/valmin/internal/runtime"
)

// PublicBuildKey is the kv row holding the last builds an update check observed.
const PublicBuildKey = "steam_public_build"

// PublicBuild is the value stored under PublicBuildKey. BuildID is the public branch's build;
// Branches is every listed branch, public included, and is empty in an observation recorded
// before branches were read.
type PublicBuild struct {
	BuildID    string        `json:"build_id"`
	Branches   []SteamBranch `json:"branches,omitempty"`
	ObservedAt time.Time     `json:"observed_at"`
}

// BranchBuilds is the observed build of each listed branch, by name.
func (p *PublicBuild) BranchBuilds() map[string]string {
	out := map[string]string{}
	if p.BuildID != "" {
		out[PublicBranch] = p.BuildID
	}
	for _, b := range p.Branches {
		out[b.Name] = b.BuildID
	}
	return out
}

// QueryBuilds reads Steam metadata in an isolated container with no instance mounts. The
// result's ObservedAt is left for the caller to stamp.
func QueryBuilds(ctx context.Context, rt runtime.Runtime, image, hostDataRoot string) (PublicBuild, error) {
	var output steamOutput
	code, err := runtime.RunThrowaway(ctx, rt, &runtime.ThrowawaySpec{
		Purpose: "steam-metadata", Root: hostDataRoot,
		Image: image, User: containerUser, Env: []string{"HOME=/tmp"},
		Cmd:    []string{"+login", "anonymous", "+app_info_print", AppID, "+quit"},
		Stdout: &output, Stderr: &output,
	})
	if err != nil {
		return PublicBuild{}, fmt.Errorf("query Steam metadata: %w", err)
	}
	if code != 0 {
		return PublicBuild{}, fmt.Errorf("steam metadata query exited %d", code)
	}
	branches, err := SteamBranches(output.String())
	if err != nil {
		return PublicBuild{}, err
	}
	return PublicBuild{BuildID: branches[0].BuildID, Branches: branches}, nil
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
