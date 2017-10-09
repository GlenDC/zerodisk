package storage

import (
	"context"
	"io"
	"net"
	"sync"

	"github.com/zero-os/0-Disk/config"
	"github.com/zero-os/0-Disk/log"
	"github.com/zero-os/0-Disk/nbd/ardb"
)

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

// TemplateCluster creates a template cluster using a config source.
// It supports hot reloading of the configuration,
// as well as the fact that the Cluster might not contain any servers at all.
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
	tsc.mux.RLock()
	defer tsc.mux.RUnlock()

	// compute server index of first available server
	serverIndex, err := ardb.FindFirstServerIndex(tsc.serverCount, tsc.serverIsOnline)
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

// DoFor implements StorageCluster.DoFor
func (tsc *TemplateCluster) DoFor(objectIndex int64, action ardb.StorageAction) (reply interface{}, err error) {
	tsc.mux.RLock()
	defer tsc.mux.RUnlock()

	// compute server index of first available server
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

// TODO: clean up
func (tsc *TemplateCluster) spawnConfigReloader(ctx context.Context, cs config.Source) error {
	ctx, tsc.cancel = context.WithCancel(context.Background())

	vdiskNBDRefCh, err := config.WatchVdiskNBDConfig(ctx, cs, tsc.vdiskID)
	if err != nil {
		return err
	}

	vdiskNBDConfig := <-vdiskNBDRefCh
	templateClusterID := vdiskNBDConfig.TemplateStorageClusterID

	var templateClusterCfg config.StorageClusterConfig
	var templateClusterCh <-chan config.StorageClusterConfig

	templateCtx, templateCancel := context.WithCancel(ctx)

	if templateClusterID != "" {
		templateClusterCh, err = config.WatchStorageClusterConfig(
			templateCtx, cs, vdiskNBDConfig.TemplateStorageClusterID)
		if err != nil {
			templateCancel()
			return err
		}

		templateClusterCfg = <-templateClusterCh
		err = tsc.updateStorageConfig(templateClusterCfg)
		if err != nil {
			templateCancel()
			return err
		}
	}

	go func() {
		defer templateCancel()

		for {
			select {
			case <-ctx.Done():
				return

			case vdiskNBDConfig = <-vdiskNBDRefCh:
				if vdiskNBDConfig.TemplateStorageClusterID == templateClusterID {
					continue
				}
				if vdiskNBDConfig.TemplateStorageClusterID == "" {
					templateClusterID = ""
					templateClusterCh = nil
					templateCancel()
					continue
				}

				templateCtx, temCancel := context.WithCancel(ctx)
				temCh, err := config.WatchStorageClusterConfig(
					templateCtx, cs, vdiskNBDConfig.TemplateStorageClusterID)
				if err != nil {
					temCancel()
					log.Errorf("failed to watch new template cluster config: %v", err)
					continue
				}
				templateClusterCh = temCh
				templateCancel()
				templateCancel = temCancel

			case templateClusterCfg = <-templateClusterCh:
				err = tsc.updateStorageConfig(templateClusterCfg)
				if err != nil {
					log.Errorf("failed to update new template cluster config: %v", err)
				}
			}
		}
	}()

	return nil
}

// updateStorageConfig overwrites the currently used storage config,
// iff the given config is valid.
func (tsc *TemplateCluster) updateStorageConfig(cfg config.StorageClusterConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

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
