// Package control owns the jobs that change an instance: their submission and claims, their
// runners, crash recovery of interrupted work, and the supervisor that reconciles what Docker
// reports with what the database records.
package control

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/valminhq/valmin/internal/config"
	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/manager"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// defaultPluginLoadWindow bounds the optional plugin-count evidence after readiness.
const defaultPluginLoadWindow = 5 * time.Second

// Notifier records the notifications instance jobs owe.
type Notifier interface {
	NotifyUnexpectedStop(ctx context.Context, inst *store.Instance, to, reason string)
	NotifyPublicBuild(ctx context.Context, previous, observed string) func(context.Context, *sql.Tx) error
}

// Deps are the collaborators control's components share.
type Deps struct {
	DB          *store.DB
	Engine      *jobs.Engine
	Runtime     runtime.Runtime
	Keeper      *crypto.Keeper
	Streams     *instance.Streams
	Snapshotter *Snapshotter
	// Installer is the mod engine. Nil leaves definition chains unable to install mods and
	// game updates unable to put installed mods back.
	Installer *manager.Installer
	// Notifier may be nil, in which case nothing is notified.
	Notifier Notifier
	// PublishState announces a state change the supervisor observed. It may be nil.
	PublishState func(instanceID, state string, restartRequired bool)
	// ReadMods reads the installed mods a clone copies to its destination.
	ReadMods func(ctx context.Context, inst *store.Instance) ([]store.InstanceMod, error)
}

// Components is every instance job component, built once and wired to each other.
type Components struct {
	Starter       *Starter
	Stopper       *Stopper
	Restarter     *Restarter
	Backupper     *Backupper
	Pruner        *Pruner
	Provisioner   *Provisioner
	Deleter       *Deleter
	Cloner        *Cloner
	Restorer      *Restorer
	GameUpdater   *GameUpdater
	UpdateChecker *UpdateChecker
	SetupJobs     *SetupJobs
	SetupState    *SetupState
	Snapshotter   *Snapshotter
	Operations    *Operations
	Supervisor    *Supervisor
}

// New builds the components from cfg and d. It refuses missing dependencies and a stop
// timeout that would make every stop an immediate kill.
func New(cfg *config.Config, d *Deps) (*Components, error) {
	switch {
	case d.DB == nil, d.Engine == nil, d.Runtime == nil, d.Keeper == nil, d.Streams == nil,
		d.Snapshotter == nil, d.ReadMods == nil:
		return nil, errors.New("control: a required dependency is missing")
	case cfg.Game.StopTimeout.Std() <= 0:
		return nil, errors.New("control: game.stop_timeout must be positive")
	}
	dataRoot, hostRoot := cfg.Data.Root, cfg.Data.HostRoot
	stopTimeout, readyTimeout := cfg.Game.StopTimeout.Std(), cfg.Jobs.ReadyTimeout.Std()

	c := &Components{Snapshotter: d.Snapshotter}
	c.Starter = &Starter{
		DB: d.DB, Engine: d.Engine, Runtime: d.Runtime, Keeper: d.Keeper,
		HostRoot: hostRoot, Image: cfg.Game.Image, Network: cfg.Game.Network,
		StopTimeout: stopTimeout, ReadySettle: cfg.Jobs.ReadySettle.Std(), ReadyTimeout: readyTimeout,
		PluginLoadWindow: defaultPluginLoadWindow,
	}
	c.Stopper = &Stopper{
		DB: d.DB, Engine: d.Engine, Runtime: d.Runtime, StopTimeout: stopTimeout, ReadyTimeout: readyTimeout,
	}
	c.Pruner = &Pruner{DB: d.DB, Engine: d.Engine}
	c.Backupper = &Backupper{
		DB: d.DB, Engine: d.Engine, Runtime: d.Runtime, DataRoot: dataRoot,
		Stopper: c.Stopper, Starter: c.Starter,
	}
	c.Restarter = &Restarter{Engine: d.Engine, Starter: c.Starter, Stopper: c.Stopper, Backupper: c.Backupper}
	c.Operations = &Operations{DB: d.DB, Engine: d.Engine, Starter: c.Starter}
	if d.Installer != nil {
		c.Operations.Mods = d.Installer
	}
	c.Provisioner = &Provisioner{
		DB: d.DB, Engine: d.Engine, Runtime: d.Runtime,
		DataRoot: dataRoot, HostRoot: hostRoot, SteamCMDImage: cfg.Game.SteamCMDImage,
		Image: cfg.Game.Image, Network: cfg.Game.Network, StopTimeout: stopTimeout,
		AdvanceChain: c.Operations.Advance,
	}
	c.Deleter = &Deleter{Engine: d.Engine, Runtime: d.Runtime, DataRoot: dataRoot}
	c.Cloner = &Cloner{
		DB: d.DB, Engine: d.Engine, Keeper: d.Keeper, Runtime: d.Runtime, Snapshotter: d.Snapshotter,
		HostRoot: hostRoot, Image: cfg.Game.Image, Network: cfg.Game.Network, StopTimeout: stopTimeout,
		ReadMods: d.ReadMods,
	}
	c.Restorer = &Restorer{Snapshotter: d.Snapshotter}
	c.GameUpdater = &GameUpdater{
		Engine: d.Engine, Runtime: d.Runtime, Config: cfg, Snapshotter: d.Snapshotter,
		Installer: d.Installer,
	}
	c.UpdateChecker = &UpdateChecker{
		DB: d.DB, Engine: d.Engine, Runtime: d.Runtime, Config: cfg, Notifier: d.Notifier,
	}
	c.SetupJobs = &SetupJobs{
		DB: d.DB, Runtime: d.Runtime, DataRoot: dataRoot, Keeper: d.Keeper, Snapshotter: d.Snapshotter,
	}
	c.SetupState = &SetupState{DB: d.DB}
	c.Supervisor = &Supervisor{
		DB: d.DB, Engine: d.Engine, Runtime: d.Runtime, Keeper: d.Keeper, Streams: d.Streams,
		DataRoot: dataRoot, HostRoot: hostRoot, StopTimeout: stopTimeout, ReadyTimeout: readyTimeout,
		PublishState: d.PublishState, Notifier: d.Notifier,
		Starter: c.Starter, Provisioner: c.Provisioner, Deleter: c.Deleter, UpdateChecker: c.UpdateChecker,
		crash: instance.NewCrashLoop(), owedStops: make(map[string]string),
	}
	if d.Installer != nil {
		queue := &ModQueue{DB: d.DB, Installer: d.Installer, Starter: c.Starter}
		c.Supervisor.ModQueue, c.Restarter.ModQueue = queue, queue
	}
	return c, nil
}
