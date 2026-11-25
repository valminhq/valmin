package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/command"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/instance/control"
	modconfig "github.com/valminhq/valmin/internal/mods/config"
	"github.com/valminhq/valmin/internal/mods/fsutil"
	"github.com/valminhq/valmin/internal/store"
)

// templateFile is one config file the panel changed: its whole content and the version its
// changes are measured against. TooLarge marks a .cfg over instance.MaxConfigSize, whose changed
// settings a template code carries but which a template file leaves out.
type templateFile struct {
	File     string
	Content  []byte
	Baseline []byte
	TooLarge bool
}

// templateLeftOut is a changed config file templates leave out, and why, worded for people.
type templateLeftOut struct {
	File   string `json:"file"`
	Reason string `json:"reason"`
}

// templateFilesView is GET /instances/{id}/manifest/files: the config files a template file
// carries, and the changed ones templates leave out.
type templateFilesView struct {
	Carried []templateCarriedView `json:"carried"`
	LeftOut []templateLeftOut     `json:"left_out"`
}

// templateCarriedView is one config file templates carry.
type templateCarriedView struct {
	File      string `json:"file"`
	SizeBytes int    `json:"size_bytes"`
}

const (
	leftOutOutside  = "changed outside the panel"
	leftOutTooLarge = "larger than 1 MiB"
	// leftOutLargeCfg is a changed .cfg over 1 MiB: left out of template files only.
	leftOutLargeCfg = "larger than 1 MiB, so template files leave it out; template codes carry its changed settings"
)

// templateFiles sorts inst's config tree, apart from the RCON file, into the files the panel
// changed from their baseline, and the changed files templates leave out. A
// file's baseline is its baseline revision once the panel or a setup has written it, or for a
// file with no history the `.orig` copy an earlier release kept; a file with neither has no
// changes the panel made.
func templateFiles(
	ctx context.Context, db *store.DB, inst *store.Instance,
) (carried []templateFile, leftOut []templateLeftOut, err error) {
	dir, err := instance.OpenConfigDir(inst.DataDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = dir.Close() }()
	listing, err := instance.ConfigTree(dir)
	if err != nil {
		return nil, nil, err
	}
	histories, err := db.ConfigHistories(ctx, inst.ID)
	if err != nil {
		return nil, nil, err
	}
	for _, name := range listing.TooLarge {
		if histories[name].Written {
			leftOut = append(leftOut, templateLeftOut{File: name, Reason: leftOutTooLarge})
		}
	}
	for _, f := range listing.Files {
		if f.Path == command.ConfigFile {
			continue
		}
		file, reason, err := readTemplateFile(ctx, db, inst.ID, dir, f.Path, histories)
		switch {
		case err != nil:
			return nil, nil, err
		case reason != "":
			leftOut = append(leftOut, templateLeftOut{File: f.Path, Reason: reason})
		case file != nil:
			carried = append(carried, *file)
			if file.TooLarge {
				reason := leftOutTooLarge
				if control.CarriedConfig(f.Path) {
					reason = leftOutLargeCfg
				}
				leftOut = append(leftOut, templateLeftOut{File: f.Path, Reason: reason})
			}
		}
	}
	return carried, leftOut, nil
}

// readTemplateFile reads the config file at name as templateFiles judges it: the file when the
// panel changed it, the reason a changed file is left out, such as one that differs from its
// baseline only through changes made outside the panel, or neither for an unchanged file.
func readTemplateFile(
	ctx context.Context, db *store.DB, instanceID string, dir *os.Root, name string,
	histories map[string]store.ConfigHistory,
) (file *templateFile, reason string, err error) {
	history, tracked := histories[name]
	if tracked && !history.Written && !history.Outside {
		return nil, "", nil
	}
	baseline, ok, err := templateBaseline(ctx, db, instanceID, dir, name, tracked)
	if err != nil || !ok {
		return nil, "", err
	}
	content, _, err := fsutil.ReadRegularIn(dir, name)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fsutil.ErrNotRegular) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("read config %s: %w", name, err)
	}
	switch {
	case bytes.Equal(content, baseline):
		return nil, "", nil
	case tracked && !history.Written:
		return nil, leftOutOutside, nil
	}
	return &templateFile{
		File: name, Content: content, Baseline: baseline, TooLarge: len(content) > instance.MaxConfigSize,
	}, "", nil
}

// templateBaseline is the version file's changes are measured against: its baseline revision
// when tracked, its `.orig` copy otherwise. ok is false when there is none.
func templateBaseline(
	ctx context.Context, db *store.DB, instanceID string, dir *os.Root, file string, tracked bool,
) (baseline []byte, ok bool, err error) {
	if tracked {
		rev, err := db.ConfigBaseline(ctx, instanceID, file)
		if err != nil || rev == nil {
			return nil, false, err
		}
		return rev.Content, true, nil
	}
	orig, _, err := fsutil.ReadRegularIn(dir, file+modconfig.OriginalSuffix)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fsutil.ErrNotRegular) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read the original of %s: %w", file, err)
	}
	return orig, true, nil
}

// listTemplateFiles handles GET /instances/{id}/manifest/files, gated like the template
// exports.
func (h *Instances) listTemplateFiles(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	for _, action := range []authz.Action{authz.InstanceSettings, authz.ModsList, authz.ConfigRead} {
		if !h.Authz.Can(r.Context(), u, action, id) {
			apierr.Write(w, r, apierr.New(errcode.Forbidden))
			return
		}
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}
	carried, leftOut, err := templateFiles(r.Context(), h.DB, inst)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	view := templateFilesView{Carried: []templateCarriedView{}, LeftOut: []templateLeftOut{}}
	for _, f := range carried {
		if !f.TooLarge {
			view.Carried = append(view.Carried, templateCarriedView{File: f.File, SizeBytes: len(f.Content)})
		}
	}
	view.LeftOut = append(view.LeftOut, leftOut...)
	JSON(w, r, http.StatusOK, view)
}
