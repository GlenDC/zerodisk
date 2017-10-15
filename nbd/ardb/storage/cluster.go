package storage

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"

	"github.com/zero-os/0-Disk/config"
	"github.com/zero-os/0-Disk/log"
	"github.com/zero-os/0-Disk/nbd/ardb"
)

// TODO:
// add server states (and stub handling)
// https://github.com/zero-os/0-Disk/issues/455

// NewPrimaryCluster creates a new PrimaryCluster.
// See `PrimaryCluster` for more information.
func NewPrimaryCluster(ctx context.Context, vdiskID string, cs config.Source) (*PrimaryCluster, error) {
	primaryCluster := &PrimaryCluster{
		vdiskID: vdiskID,
		pool:    ardb.NewPool(nil),
	}
	err := primaryCluster.spawnConfigReloader(ctx, cs)
	if err != nil {
		primaryCluster.Close()
		return nil, err
	}

	return primaryCluster, nil
}

// PrimaryCluster defines a vdisk's primary cluster.
// It supports hot reloading of the configuration
// and state handling of the individual servers of a cluster.
type PrimaryCluster struct {
	vdiskID string

	servers     []config.StorageServerConfig
	serverCount int64

	pool   *ardb.Pool
	cancel context.CancelFunc

	mux sync.RWMutex
}

// Do implements StorageCluster.Do
func (pc *PrimaryCluster) Do(action ardb.StorageAction) (reply interface{}, err error) {
	pc.mux.RLock()
	defer pc.mux.RUnlock()

	// compute server index of first available server
	serverIndex, err := ardb.FindFirstServerIndex(pc.serverCount, pc.serverIsOnline)
	if err != nil {
		return nil, err
	}

	return pc.doAt(serverIndex, action)
}

// DoFor implements StorageCluster.DoFor
func (pc *PrimaryCluster) DoFor(objectIndex int64, action ardb.StorageAction) (reply interface{}, err error) {
	pc.mux.RLock()
	defer pc.mux.RUnlock()

	// compute server index for the server which maps to the given object index
	serverIndex, err := ardb.ComputeServerIndex(pc.serverCount, objectIndex, pc.serverIsOnline)
	if err != nil {
		return nil, err
	}

	return pc.doAt(serverIndex, action)
}

// execute an exuction at a given primary server
func (pc *PrimaryCluster) doAt(serverIndex int64, action ardb.StorageAction) (reply interface{}, err error) {
	// establish a connection for that serverIndex
	cfg := pc.servers[serverIndex]

	conn, err := pc.pool.Dial(cfg)
	if err == nil {
		defer conn.Close()
		reply, err = action.Do(conn)
		if err == nil {
			return reply, nil
		}
	}

	// TODO:
	// add self-healing...
	// see: https://github.com/zero-os/0-Disk/issues/445
	// and  https://github.com/zero-os/0-Disk/issues/284

	// an error has occured, broadcast it to AYS
	status := mapErrorToBroadcastStatus(err)
	log.Broadcast(
		status,
		log.SubjectStorage,
		log.ARDBServerTimeoutBody{
			Address:  cfg.Address,
			Database: cfg.Database,
			Type:     log.ARDBPrimaryServer,
			VdiskID:  pc.vdiskID,
		},
	)
	return nil, err
}

// Close any open resources
func (pc *PrimaryCluster) Close() error {
	pc.cancel()
	pc.pool.Close()
	return nil
}

// serverOperational returns true if
// a server on the given index is online.
func (pc *PrimaryCluster) serverIsOnline(index int64) bool {
	return pc.servers[index].State == config.StorageServerStateOnline
}

// spawnConfigReloader starts all needed config watchers,
// and spawns a goroutine to receive the updates.
// An error is returned in case the initial watch-creation and config-update failed.
// All future errors will be logged (and optionally broadcasted),
// without stopping this goroutine.
func (pc *PrimaryCluster) spawnConfigReloader(ctx context.Context, cs config.Source) error {
	// create the context and cancelFunc used for the master watcher.
	ctx, pc.cancel = context.WithCancel(ctx)

	// create the master watcher if possible
	vdiskNBDRefCh, err := config.WatchVdiskNBDConfig(ctx, cs, pc.vdiskID)
	if err != nil {
		return err
	}
	vdiskNBDConfig := <-vdiskNBDRefCh

	var primaryClusterCfg config.StorageClusterConfig

	// create the primary storage cluster watcher,
	// and execute the initial config update iff
	// an internal watcher is created.
	var primaryWatcher storageClusterWatcher
	clusterExists, err := primaryWatcher.SetClusterID(ctx, cs, pc.vdiskID, vdiskNBDConfig.StorageClusterID)
	if err != nil {
		return err
	}
	if !clusterExists {
		panic("primary cluster should exist on a non-error path")
	}
	primaryClusterCfg = <-primaryWatcher.Receive()
	err = pc.updatePrimaryStorageConfig(primaryClusterCfg)
	if err != nil {
		return err
	}

	// spawn the config update goroutine
	go func() {
		var ok bool
		for {
			select {
			case <-ctx.Done():
				return

			// handle clusterID reference updates
			case vdiskNBDConfig, ok = <-vdiskNBDRefCh:
				if !ok {
					return
				}

				_, err = primaryWatcher.SetClusterID(
					ctx, cs, pc.vdiskID, vdiskNBDConfig.StorageClusterID)
				if err != nil {
					log.Errorf("failed to watch new primary cluster config: %v", err)
				}

			// handle primary cluster storage updates
			case primaryClusterCfg = <-primaryWatcher.Receive():
				err = pc.updatePrimaryStorageConfig(primaryClusterCfg)
				if err != nil {
					log.Errorf("failed to update new primary cluster config: %v", err)
				}
			}
		}
	}()

	// all is operational, no error to return
	return nil
}

// updatePrimaryStorageConfig overwrites
// the currently used primary storage config,
func (pc *PrimaryCluster) updatePrimaryStorageConfig(cfg config.StorageClusterConfig) error {
	pc.mux.Lock()
	pc.servers = cfg.Servers
	pc.serverCount = int64(len(cfg.Servers))
	pc.mux.Unlock()
	return nil
}

// NewTemplateCluster creates a new TemplateCluster.
// See `TemplateCluster` for more information.
func NewTemplateCluster(ctx context.Context, vdiskID string, cs config.Source) (*TemplateCluster, error) {
	templateCluster := &TemplateCluster{
		vdiskID: vdiskID,
		pool:    ardb.NewPool(nil),
	}
	err := templateCluster.spawnConfigReloader(ctx, cs)
	if err != nil {
		templateCluster.Close()
		return nil, err
	}

	return templateCluster, nil
}

// TemplateCluster defines a vdisk'stemplate cluster (configured or not).
// It supports hot reloading of the configuration.
type TemplateCluster struct {
	vdiskID string

	servers     []config.StorageServerConfig
	serverCount int64

	pool   *ardb.Pool
	cancel context.CancelFunc

	mux sync.RWMutex
}

// Do implements StorageCluster.Do
func (tsc *TemplateCluster) Do(action ardb.StorageAction) (reply interface{}, err error) {
	return nil, ErrMethodNotSupported
}

// DoFor implements StorageCluster.DoFor
func (tsc *TemplateCluster) DoFor(objectIndex int64, action ardb.StorageAction) (reply interface{}, err error) {
	tsc.mux.RLock()
	defer tsc.mux.RUnlock()

	// ensure the template cluster is actually defined,
	// as it is created even when no clusterID is referenced,
	// just in case one would be defined via a hotreload.
	if tsc.serverCount == 0 {
		return nil, ErrClusterNotDefined
	}

	// compute server index for the server which maps to the given object index
	serverIndex, err := ardb.ComputeServerIndex(tsc.serverCount, objectIndex, tsc.serverIsOnline)
	if err != nil {
		return nil, err
	}

	// establish a connection for that serverIndex
	cfg := tsc.servers[serverIndex]
	conn, err := tsc.pool.Dial(cfg)
	if err == nil {
		defer conn.Close()
		reply, err = action.Do(conn)
		if err == nil {
			return reply, nil
		}
	}

	// an error has occured, broadcast it to AYS
	status := mapErrorToBroadcastStatus(err)
	log.Broadcast(
		status,
		log.SubjectStorage,
		log.ARDBServerTimeoutBody{
			Address:  cfg.Address,
			Database: cfg.Database,
			Type:     log.ARDBTemplateServer,
			VdiskID:  tsc.vdiskID,
		},
	)
	return nil, err
}

// Close any open resources
func (tsc *TemplateCluster) Close() error {
	tsc.cancel()
	tsc.pool.Close()
	return nil
}

// spawnConfigReloader starts all needed config watchers,
// and spawns a goroutine to receive the updates.
// An error is returned in case the initial watch-creation and config-update failed.
// All future errors will be logged (and optionally broadcasted),
// without stopping this goroutine.
func (tsc *TemplateCluster) spawnConfigReloader(ctx context.Context, cs config.Source) error {
	// create the context and cancelFunc used for the master watcher.
	ctx, tsc.cancel = context.WithCancel(ctx)

	// create the master watcher if possible
	vdiskNBDRefCh, err := config.WatchVdiskNBDConfig(ctx, cs, tsc.vdiskID)
	if err != nil {
		return err
	}
	vdiskNBDConfig := <-vdiskNBDRefCh

	// create the storage cluster watcher,
	// and execute the initial config update iff
	// an internal watcher is created.
	var watcher storageClusterWatcher
	clusterExists, err := watcher.SetClusterID(
		ctx, cs, tsc.vdiskID, vdiskNBDConfig.TemplateStorageClusterID)
	if err != nil {
		return err
	}
	var templateClusterCfg config.StorageClusterConfig
	if clusterExists {
		templateClusterCfg = <-watcher.Receive()
		err = tsc.updateStorageConfig(templateClusterCfg)
		if err != nil {
			return err
		}
	}

	// spawn the config update goroutine
	go func() {
		var ok bool
		for {
			select {
			case <-ctx.Done():
				return

			// handle clusterID reference updates
			case vdiskNBDConfig, ok = <-vdiskNBDRefCh:
				if !ok {
					return
				}

				clusterWasDefined := watcher.Defined()
				clusterExists, err = watcher.SetClusterID(
					ctx, cs, tsc.vdiskID, vdiskNBDConfig.TemplateStorageClusterID)
				if err != nil {
					log.Errorf("failed to watch new template cluster config: %v", err)
					continue
				}
				if clusterWasDefined && !clusterExists {
					// no cluster exists any longer, we need to delete the old state
					tsc.mux.Lock()
					tsc.servers, tsc.serverCount = nil, 0
					tsc.mux.Unlock()
				}

			// handle cluster storage updates
			case templateClusterCfg = <-watcher.Receive():
				err = tsc.updateStorageConfig(templateClusterCfg)
				if err != nil {
					log.Errorf("failed to update new template cluster config: %v", err)
				}
			}
		}
	}()

	// all is operational, no error to return
	return nil
}

// updateStorageConfig overwrites the currently used storage config,
// iff the given config is valid.
func (tsc *TemplateCluster) updateStorageConfig(cfg config.StorageClusterConfig) error {
	tsc.mux.Lock()
	tsc.servers = cfg.Servers
	tsc.serverCount = int64(len(cfg.Servers))
	tsc.mux.Unlock()
	return nil
}

// serverOperational returns true if
// the server on the given index is online.
func (tsc *TemplateCluster) serverIsOnline(index int64) bool {
	return tsc.servers[index].State == config.StorageServerStateOnline
}

// storageClusterWatcher is a small helper struct,
// used to (un)set a storage cluster watcher for a given clusterID.
// By centralizing this logic,
// we only have to define it once and it keeps the callee's location clean.
type storageClusterWatcher struct {
	clusterID string
	channel   <-chan config.StorageClusterConfig
	cancel    context.CancelFunc
}

// Receive an update on the returned channel by the storageClusterWatcher.
func (scw *storageClusterWatcher) Receive() <-chan config.StorageClusterConfig {
	return scw.channel
}

// Close all open resources,
// openend and managed by this storageClusterWatcher
func (scw *storageClusterWatcher) Close() {
	if scw.cancel != nil {
		scw.cancel()
	}
}

// SetCluster allows you to (over)write the current internal cluster watcher.
// If the given clusterID is equal to the already used clusterID, nothing will happen.
// If the clusterID is different but the given one is nil, the current watcher will be stopped.
// In all other cases a new watcher will be attempted to be created,
// and used if succesfull (right before cancelling the old one), or otherwise an error is returned.
// In an error case the boolean parameter indicates whether a watcher is active or not.
func (scw *storageClusterWatcher) SetClusterID(ctx context.Context, cs config.Source, vdiskID, clusterID string) (bool, error) {
	if scw.clusterID == clusterID {
		// if the given ID is equal to the one we have stored internally,
		// we have nothing to do.
		// Returning true, such that no existing cluster info is deleted by accident.
		return scw.clusterID != "", nil
	}

	// if the given clusterID is nil, but ours isn't,
	// we'll simply want to close the watcher and clean up our internal state.
	if clusterID == "" {
		scw.cancel()
		scw.cancel = nil
		scw.clusterID = ""
		return false, nil // no watcher is active, as no cluster exists
	}

	// try to create the new watcher
	ctx, cancel := context.WithCancel(ctx)
	channel, err := config.WatchStorageClusterConfig(ctx, cs, clusterID)
	if err != nil {
		cs.MarkInvalidKey(config.Key{ID: vdiskID, Type: config.KeyVdiskNBD}, vdiskID)
		cancel()
		return false, err
	}

	// close the previous watcher
	scw.Close()

	// use the new watcher and set the new state
	scw.cancel = cancel
	scw.clusterID = clusterID
	scw.channel = channel
	return true, nil // a watcher is active, because the cluster exists
}

// Defined returns `true` if this storage cluster watcher
// has an internal watcher (for an existing cluster) defined.
func (scw *storageClusterWatcher) Defined() bool {
	return scw.clusterID != ""
}

// mapErrorToBroadcastStatus maps the given error,
// returned by a `Connection` operation to a broadcast's message status.
func mapErrorToBroadcastStatus(err error) log.MessageStatus {
	if netErr, ok := err.(net.Error); ok {
		if netErr.Timeout() {
			return log.StatusServerTimeout
		}
		if netErr.Temporary() {
			return log.StatusServerTempError
		}
	} else if err == io.EOF {
		return log.StatusServerDisconnect
	}

	return log.StatusUnknownError
}

var (
	// ErrMethodNotSupported is an error returned
	// in case a method is called which is not supported by the object.
	ErrMethodNotSupported = errors.New("method is not supported")

	// ErrClusterNotDefined is an error returned
	// in case a cluster is used which is not defined.
	ErrClusterNotDefined = errors.New("ARDB storage cluster is not defined")
)
