/*
Copyright Intel Corporation

Fill this thing up
*/

package lvmsnapshotter

import (
	"context"
	"os"
	"path/filepath"

	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/platforms"
	"github.com/containerd/containerd/plugin"
	"github.com/containerd/containerd/snapshots"
	"github.com/containerd/containerd/snapshots/storage"

	"github.com/containerd/continuity/fs"
	"github.com/pkg/errors"
)

const (
	metaVolumeMountName = "contd-lvm-snapshotter-db-holder"
	metavolume          = "contd-metadata-holder"
)

func init() {
	plugin.Register(&plugin.Registration{
		Type:   plugin.SnapshotPlugin,
		ID:     "lvm",
		Config: &LVMSnapConfig{},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			ic.Meta.Platforms = append(ic.Meta.Platforms, platforms.DefaultSpec())

			config, ok := ic.Config.(*LVMSnapConfig)
			if !ok {
				return nil, errors.New("Invalid LVM config")
			}
			if err := config.Validate(ic.Root); err != nil {
				return nil, errors.Wrap(err, "Unable to validate config")
			}

			return NewSnapshotter(ic.Context, config)
		},
	})
}

type snapshotter struct {
	config      *LVMSnapConfig
	ms          *storage.MetaStore
	metaVolPath string
}

// NewSnapshotter returns a Snapshotter which copies layers on the underlying
// file system. A metadata file is stored under the root.
func NewSnapshotter(ctx context.Context, config *LVMSnapConfig) (snapshots.Snapshotter, error) {
	var err error

	if _, err = checkVG(config.VgName); err != nil {
		return nil, errors.Wrap(err, "VG not found")
	}

	_, err = checkLV(config.VgName, config.ThinPool)
	if err != nil {
		return nil, errors.Wrap(err, "LV not found")
	}

	_, err = checkLV(config.VgName, metavolume)
	if err != nil {
		// Create a volume to hold the metadata.db file.
		if _, err = createLVMVolume(metavolume, config.VgName, config.ThinPool, config.ImageSize, "", snapshots.KindUnknown); err != nil {
			return nil, errors.Wrap(err, "Unable to create metadata holding volume")
		}
	}
	if _, err = toggleactivateLV(config.VgName, metavolume, true); err != nil {
		return nil, errors.Wrap(err, "Unable to activate metavolume")
	}

	metavolpath := filepath.Join(config.RootPath, metaVolumeMountName)
	if errdir := os.MkdirAll(metavolpath, 0700); errdir != nil {
		return nil, errdir
	}

	metamount := []mount.Mount{
		{
			Source:  "/dev/" + config.VgName + "/" + metavolume,
			Type:    config.FsType,
			Options: []string{},
		},
	}

	if err = mount.All(metamount, metavolpath); err != nil {
		return nil, err
	}
	ms, err := storage.NewMetaStore(filepath.Join(metavolpath, "metadata.db"))
	if err != nil {
		return nil, err
	}

	return &snapshotter{
		config:      config,
		ms:          ms,
		metaVolPath: metavolpath,
	}, nil
}

// Stat returns the info for an active or committed snapshot by name or
// key.
//
// Should be used for parent resolution, existence checks and to discern
// the kind of snapshot.
func (o *snapshotter) Stat(ctx context.Context, key string) (snapshots.Info, error) {
	log.G(ctx).Debugf("Stat called for: %s", key)
	ctx, t, err := o.ms.TransactionContext(ctx, false)
	if err != nil {
		return snapshots.Info{}, err
	}
	defer func() {
		if rerr := t.Rollback(); rerr != nil {
			log.G(ctx).WithError(rerr).Warn("Failed to rollback transaction")
		}
	}()
	_, info, _, err := storage.GetInfo(ctx, key)
	if err != nil {
		return snapshots.Info{}, err
	}

	return info, nil
}

func (o *snapshotter) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) (snapshots.Info, error) {
	log.G(ctx).Debugf("Update called for : %+v", info)
	ctx, t, err := o.ms.TransactionContext(ctx, true)
	if err != nil {
		return snapshots.Info{}, err
	}

	info, err = storage.UpdateInfo(ctx, info, fieldpaths...)
	if err != nil {
		if rerr := t.Rollback(); rerr != nil {
			log.G(ctx).WithError(rerr).Warn("Failed to rollback transaction")
		}
		return snapshots.Info{}, err
	}

	if err := t.Commit(); err != nil {
		return snapshots.Info{}, err
	}

	return info, nil
}

func (o *snapshotter) Usage(ctx context.Context, key string) (snapshots.Usage, error) {
	log.G(ctx).Debugf("Usage of key %+v", key)
	ctx, t, err := o.ms.TransactionContext(ctx, false)
	var s storage.Snapshot
	var du fs.Usage
	if err != nil {
		return snapshots.Usage{}, err
	}
	defer func() {
		if rerr := t.Rollback(); err != nil {
			log.G(ctx).WithError(rerr).Warn("Failed to rollback transaction")
		}
	}()
	id, info, usage, err := storage.GetInfo(ctx, key)
	if err != nil {
		return snapshots.Usage{}, err
	}

	if info.Kind == snapshots.KindActive {
		if s, err = storage.GetSnapshot(ctx, key); err != nil {
			return snapshots.Usage{}, err
		}
		mounts := o.mounts(s)
		if err = mount.WithTempMount(ctx, mounts, func(root string) error {
			if du, err = fs.DiskUsage(ctx, o.getSnapshotDir(id)); err != nil {
				return err
			}
			usage = snapshots.Usage(du)
			return nil
		}); err != nil {
			return snapshots.Usage{}, err
		}
	}

	log.G(ctx).Debugf("Usage of key %s is %+v", key, usage)
	return usage, nil
}

func (o *snapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	log.G(ctx).Debugf("Preparing snapshot for key %s with parent %s", key, parent)
	return o.createSnapshot(ctx, snapshots.KindActive, key, parent, opts)
}

func (o *snapshotter) View(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	log.G(ctx).Debugf("Viewing snapshot for key %s with parent %s", key, parent)
	return o.createSnapshot(ctx, snapshots.KindView, key, parent, opts)
}

// Mounts returns the mounts for the transaction identified by key. Can be
// called on an read-write or readonly transaction.
//
// This can be used to recover mounts after calling View or Prepare.
func (o *snapshotter) Mounts(ctx context.Context, key string) ([]mount.Mount, error) {
	log.G(ctx).Debugf("Finding mounts for key %s", key)
	ctx, t, err := o.ms.TransactionContext(ctx, false)
	if err != nil {
		return nil, err
	}
	s, err := storage.GetSnapshot(ctx, key)
	if rerr := t.Rollback(); rerr != nil {
		log.G(ctx).WithError(rerr).Warn("failed to rollback transaction")
	}
	if err != nil {
		return nil, errors.Wrap(err, "failed to get snapshot mount")
	}
	log.G(ctx).Debugf("Mounts for key %s is %+v", key, o.mounts(s))
	return o.mounts(s), nil
}

func (o *snapshotter) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) error {
	log.G(ctx).Debugf("Commit snapshot for key %s", key)
	ctx, t, err := o.ms.TransactionContext(ctx, true)
	var du fs.Usage
	var usage snapshots.Usage
	if err != nil {
		return err
	}

	id, _, _, err := storage.GetInfo(ctx, key)
	if err != nil {
		return err
	}

	s, err := storage.GetSnapshot(ctx, key)
	mounts := o.mounts(s)
	if err = mount.WithTempMount(ctx, mounts, func(root string) error {
		if du, err = fs.DiskUsage(ctx, o.getSnapshotDir(id)); err != nil {
			return err
		}
		usage = snapshots.Usage(du)
		return nil
	}); err != nil {
		return err
	}

	if _, err = storage.CommitActive(ctx, key, name, usage, opts...); err != nil {
		if rerr := t.Rollback(); rerr != nil {
			log.G(ctx).WithError(rerr).Warn("failed to rollback transaction")
		}
		return errors.Wrap(err, "failed to commit snapshot")
	}
	if _, err = changepermLV(o.config.VgName, id, true); err != nil {
		return errors.Wrap(err, "Failed to change permissions on volume")
	}

	// Deactivate the volume in LVM to free up /dev/dm-XX names on the host
	if _, err = toggleactivateLV(o.config.VgName, id, false); err != nil {
		return errors.Wrap(err, "Failed to change permissions on volume")
	}

	err = t.Commit()
	if err != nil {
		log.G(ctx).WithError(err).Warn("Transaction commit failed")
		if _, derr := removeLVMVolume(o.config.VgName, id); derr != nil {
			log.G(ctx).WithError(derr).Warn("Unable to delete volume")
		}
		return err
	}
	return nil
}

// Remove abandons the transaction identified by key. All resources
// associated with the key will be removed.
func (o *snapshotter) Remove(ctx context.Context, key string) (err error) {
	log.G(ctx).Debugf("Remove contents of key %s", key)
	ctx, t, err := o.ms.TransactionContext(ctx, true)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil && t != nil {
			if rerr := t.Rollback(); rerr != nil {
				log.G(ctx).WithError(rerr).Warn("failed to rollback transaction")
			}
		}
	}()

	id, _, err := storage.Remove(ctx, key)
	if err != nil {
		return errors.Wrap(err, "failed to remove")
	}

	_, err = removeLVMVolume(id, o.config.VgName)
	if err != nil {
		return errors.Wrap(err, "failed to deletel LVM volume")
	}

	err = t.Commit()
	t = nil
	if err != nil {
		return errors.Wrap(err, "failed to commit")
	}

	return nil
}

// Walk the committed snapshots.
func (o *snapshotter) Walk(ctx context.Context, fn func(context.Context, snapshots.Info) error) error {
	log.G(ctx).Debugf("Walk through %+v", ctx)
	ctx, t, err := o.ms.TransactionContext(ctx, false)
	if err != nil {
		return err
	}
	defer func() {
		if rerr := t.Rollback(); rerr != nil {
			log.G(ctx).WithError(rerr).Warn("Failed to rollback transaction")
		}
	}()
	return storage.WalkInfo(ctx, fn)
}

func (o *snapshotter) createSnapshot(ctx context.Context, kind snapshots.Kind, key, parent string, opts []snapshots.Opt) (_ []mount.Mount, err error) {

	pvol := ""
	ctx, t, err := o.ms.TransactionContext(ctx, true)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil && t != nil {
			if rerr := t.Rollback(); rerr != nil {
				log.G(ctx).WithError(rerr).Warn("Failed to rollback transaction")
			}
		}
	}()

	s, err := storage.CreateSnapshot(ctx, kind, key, parent, opts...)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create snapshot")
	}

	if len(s.ParentIDs) == 0 {
		// Create a new logical volume without a base snapshot
		pvol = ""
	} else {
		// Create a snapshot from the parent
		pvol = s.ParentIDs[0]
	}
	if _, err := createLVMVolume(s.ID, o.config.VgName, o.config.ThinPool, o.config.ImageSize, pvol, kind); err != nil {
		log.G(ctx).WithError(err).Warn("Unable to create volume")
		return nil, errors.Wrap(err, "Unable to create volume")
	}

	if err := t.Commit(); err != nil {
		return nil, err
	}

	log.G(ctx).Debugf("Mounts for key %s is %+v", key, o.mounts(s))
	return o.mounts(s), nil
}

func (o *snapshotter) getSnapshotDir(id string) string {
	return filepath.Join("/dev", o.config.VgName, id)
}

func (o *snapshotter) mounts(s storage.Snapshot) []mount.Mount {
	var (
		roFlag string
		source string
	)

	if s.Kind == snapshots.KindView {
		roFlag = "-oro"
	}

	source = o.getSnapshotDir(s.ID)
	return []mount.Mount{
		{
			Source: source,
			Type:   o.config.FsType,
			Options: []string{
				roFlag,
			},
		},
	}
}

// Close closes the snapshotter
func (o *snapshotter) Close() error {
	var err = o.ms.Close()
	if err != nil {
		return err
	}
	err = mount.UnmountAll(o.metaVolPath, 0)
	if err != nil {
		return err
	}
	return nil
}
