package control

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// Orphan is a managed container no instance row claims.
type Orphan struct {
	ContainerID string
	Name        string
	InstanceID  string
	BasePort    int
	Running     bool
}

// managedContainers lists panel containers by their instance label.
func managedContainers(ctx context.Context, rt runtime.Runtime) (map[string]*runtime.Container, error) {
	containers, err := rt.List(ctx, map[string]string{instance.LabelManaged: "true"})
	if err != nil {
		return nil, fmt.Errorf("list managed containers: %w", err)
	}
	byInstanceID := make(map[string]*runtime.Container, len(containers))
	for i := range containers {
		c := &containers[i]
		id := c.Labels[instance.LabelInstanceID]
		if id == "" {
			slog.WarnContext(ctx, "managed container carries no instance id label",
				slog.String("container_id", c.ID))
			continue
		}
		byInstanceID[id] = c
	}
	return byInstanceID, nil
}

// ListOrphans reports managed containers unclaimed by an instance row.
func ListOrphans(ctx context.Context, db *store.DB, rt runtime.Runtime) ([]Orphan, error) {
	byInstanceID, err := managedContainers(ctx, rt)
	if err != nil {
		return nil, err
	}
	instances, err := db.ListInstances(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("list orphans: %w", err)
	}
	claimedIDs := make(map[string]bool, len(instances))
	claimedContainers := make(map[string]bool, len(instances))
	for i := range instances {
		claimedIDs[instances[i].ID] = true
		if instances[i].ContainerID != nil {
			claimedContainers[*instances[i].ContainerID] = true
		}
	}

	orphans := []Orphan{}
	for instanceID, c := range byInstanceID {
		if claimedIDs[instanceID] || claimedContainers[c.ID] {
			continue
		}
		basePort, _ := strconv.Atoi(c.Labels[instance.LabelBasePort])
		orphans = append(orphans, Orphan{
			ContainerID: c.ID, Name: c.Name, InstanceID: instanceID,
			BasePort: basePort, Running: c.Running,
		})
	}
	return orphans, nil
}
