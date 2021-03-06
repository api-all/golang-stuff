// Copyright 2012, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package wrangler

import (
	"fmt"

	log "github.com/golang/glog"
	tm "github.com/youtube/vitess/go/vt/tabletmanager"
	"github.com/youtube/vitess/go/vt/topo"
)

// Tablet related methods for wrangler

// InitTablet creates or updates a tablet. If no parent is specified
// in the tablet, and the tablet has a slave type, we will find the
// appropriate parent. If createShardAndKeyspace is true and the
// parent keyspace or shard don't exist, they will be created.  If
// update is true, and a tablet with the same ID exists, update it.
// If Force is true, and a tablet with the same ID already exists, it
// will be scrapped and deleted, and then recreated.
func (wr *Wrangler) InitTablet(tablet *topo.Tablet, force, createShardAndKeyspace, update bool) error {
	if err := tablet.Complete(); err != nil {
		return err
	}
	if tablet.Parent.IsZero() && tablet.Type.IsSlaveType() {
		parentAlias, err := wr.getMasterAlias(tablet.Keyspace, tablet.Shard)
		if err != nil {
			return err
		}
		tablet.Parent = parentAlias
	}

	if tablet.IsInReplicationGraph() {
		// create the parent keyspace and shard if needed
		if createShardAndKeyspace {
			if err := wr.ts.CreateKeyspace(tablet.Keyspace); err != nil && err != topo.ErrNodeExists {
				return err
			}

			if err := topo.CreateShard(wr.ts, tablet.Keyspace, tablet.Shard); err != nil && err != topo.ErrNodeExists {
				return err
			}
		}

		// get the shard, checks KeyRange is the same
		si, err := wr.ts.GetShard(tablet.Keyspace, tablet.Shard)
		if err != nil {
			return fmt.Errorf("Missing parent shard, use -parent option to create it, or CreateKeyspace / CreateShard")
		}
		if si.KeyRange != tablet.KeyRange {
			return fmt.Errorf("Shard %v/%v has a different KeyRange: %v != %v", tablet.Keyspace, tablet.Shard, si.KeyRange, tablet.KeyRange)
		}
	}

	err := topo.CreateTablet(wr.ts, tablet)
	if err != nil && err == topo.ErrNodeExists {
		// Try to update nicely, but if it fails fall back to force behavior.
		if update {
			oldTablet, err := wr.ts.GetTablet(tablet.Alias())
			if err != nil {
				log.Warningf("failed reading tablet %v: %v", tablet.Alias(), err)
			} else {
				if oldTablet.Keyspace == tablet.Keyspace && oldTablet.Shard == tablet.Shard {
					*(oldTablet.Tablet) = *tablet
					err := topo.UpdateTablet(wr.ts, oldTablet)
					if err != nil {
						log.Warningf("failed updating tablet %v: %v", tablet.Alias(), err)
					} else {
						return nil
					}
				}
			}
		}
		if force {
			if _, err = wr.Scrap(tablet.Alias(), force, false); err != nil {
				log.Errorf("failed scrapping tablet %v: %v", tablet.Alias(), err)
				return err
			}
			if err := wr.ts.DeleteTablet(tablet.Alias()); err != nil {
				// we ignore this
				log.Errorf("failed deleting tablet %v: %v", tablet.Alias(), err)
			}
			return topo.CreateTablet(wr.ts, tablet)
		}
	}
	return err
}

// Scrap a tablet. If force is used, we write to topo.Server
// directly and don't remote-execute the command.
func (wr *Wrangler) Scrap(tabletAlias topo.TabletAlias, force, skipRebuild bool) (actionPath string, err error) {
	// load the tablet, see if we'll need to rebuild
	ti, err := wr.ts.GetTablet(tabletAlias)
	if err != nil {
		return "", err
	}
	rebuildRequired := ti.Tablet.IsServingType()

	if force {
		err = tm.Scrap(wr.ts, ti.Alias(), force)
	} else {
		actionPath, err = wr.ai.Scrap(ti.Alias())
	}
	if err != nil {
		return "", err
	}

	if !rebuildRequired {
		log.Infof("Rebuild not required")
		return
	}
	if skipRebuild {
		log.Warningf("Rebuild required, but skipping it")
		return
	}

	// wait for the remote Scrap if necessary
	if actionPath != "" {
		err = wr.ai.WaitForCompletion(actionPath, wr.actionTimeout())
		if err != nil {
			return "", err
		}
	}

	// and rebuild the original shard / keyspace
	return "", wr.RebuildShardGraph(ti.Keyspace, ti.Shard, []string{ti.Cell})
}

// Change the type of tablet and recompute all necessary derived paths in the
// serving graph.
// force: Bypass the vtaction system and make the data change directly, and
// do not run the remote hooks
func (wr *Wrangler) ChangeType(tabletAlias topo.TabletAlias, dbType topo.TabletType, force bool) error {
	// Load tablet to find keyspace and shard assignment.
	// Don't load after the ChangeType which might have unassigned
	// the tablet.
	ti, err := wr.ts.GetTablet(tabletAlias)
	if err != nil {
		return err
	}
	rebuildRequired := ti.Tablet.IsServingType()

	if force {
		// with --force, we do not run any hook
		err = tm.ChangeType(wr.ts, tabletAlias, dbType, false)
	} else {
		// the remote action will run the hooks
		var actionPath string
		actionPath, err = wr.ai.ChangeType(tabletAlias, dbType)
		// You don't have a choice - you must wait for
		// completion before rebuilding.
		if err == nil {
			err = wr.ai.WaitForCompletion(actionPath, wr.actionTimeout())
		}
	}

	if err != nil {
		return err
	}

	// we rebuild if the tablet was serving, or if it is now
	var keyspaceToRebuild string
	var shardToRebuild string
	var cellToRebuild string
	if rebuildRequired {
		keyspaceToRebuild = ti.Keyspace
		shardToRebuild = ti.Shard
		cellToRebuild = ti.Cell
	} else {
		// re-read the tablet, see if we become serving
		ti, err := wr.ts.GetTablet(tabletAlias)
		if err != nil {
			return err
		}
		if ti.Tablet.IsServingType() {
			rebuildRequired = true
			keyspaceToRebuild = ti.Keyspace
			shardToRebuild = ti.Shard
			cellToRebuild = ti.Cell
		}
	}

	if rebuildRequired {
		if err := wr.RebuildShardGraph(keyspaceToRebuild, shardToRebuild, []string{cellToRebuild}); err != nil {
			return err
		}
	}
	return nil
}

// same as ChangeType, but assume we already have the shard lock,
// and do not have the option to force anything
// FIXME(alainjobart): doesn't rebuild the Keyspace, as that part has locks,
// so the local serving graphs will be wrong. To do that, I need to refactor
// some code, might be a bigger change.
// Mike says: Updating the shard should be good enough. I'm debating dropping the entire
// keyspace rollup, since I think that is adding complexity and feels like it might
// be a premature optimization.
func (wr *Wrangler) changeTypeInternal(tabletAlias topo.TabletAlias, dbType topo.TabletType) error {
	ti, err := wr.ts.GetTablet(tabletAlias)
	if err != nil {
		return err
	}
	rebuildRequired := ti.Tablet.IsServingType()

	// change the type
	actionPath, err := wr.ai.ChangeType(ti.Alias(), dbType)
	if err != nil {
		return err
	}
	err = wr.ai.WaitForCompletion(actionPath, wr.actionTimeout())
	if err != nil {
		return err
	}

	// rebuild if necessary
	if rebuildRequired {
		err = wr.rebuildShard(ti.Keyspace, ti.Shard, []string{ti.Cell})
		if err != nil {
			return err
		}
		// FIXME(alainjobart) We already have the lock on one shard, so this is not
		// possible. But maybe it's not necessary anyway.
		// We could pass in a shard path we already have the lock on, and skip it?
		//		err = wr.rebuildKeyspace(ti.Keyspace)
		//		if err != nil {
		//			return err
		//		}
	}
	return nil
}
