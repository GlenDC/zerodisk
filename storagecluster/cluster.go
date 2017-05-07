package storagecluster

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/g8os/blockstor/nbdserver/config"
	log "github.com/glendc/go-mini-log"
)

// NewClusterClientFactory creates a ClusterClientFactory.
func NewClusterClientFactory(configPath string, logger log.Logger) (*ClusterClientFactory, error) {
	if configPath == "" {
		return nil, errors.New("NewClusterClientFactory requires a non-empty config path")
	}
	if logger == nil {
		logger = log.New(os.Stderr, "", log.LstdFlags)
	}

	return &ClusterClientFactory{
		configPath: configPath,
		requestCh:  make(chan string),
		responseCh: make(chan clusterClientResponse),
		logger:     logger,
	}, nil
}

// ClusterClientFactory allows for the creation of ClusterClients.
type ClusterClientFactory struct {
	configPath string
	// used for creation of storage cluster clients
	requestCh  chan string
	responseCh chan clusterClientResponse
	// used for internal logging purposes
	logger log.Logger
}

// NewClient returns a new ClusterClient.
func (f *ClusterClientFactory) NewClient(vdiskID string) (cc *ClusterClient, err error) {
	if vdiskID == "" {
		err = errors.New("ClusterClient requires a non-empty vdiskID")
		return
	}

	f.requestCh <- vdiskID
	resp := <-f.responseCh

	cc = resp.Client
	err = resp.Error
	return
}

// Listen to incoming creation requests (send by the NewClient method)
func (f *ClusterClientFactory) Listen(ctx context.Context) {
	for {
		select {
		// wait for a request
		case vdiskID := <-f.requestCh:
			cc, err := NewClusterClient(
				ClusterClientConfig{
					ConfigPath: f.configPath,
					VdiskID:    vdiskID,
				},
				f.logger,
			)
			if err != nil {
				// couldn't create cc, early exit
				f.responseCh <- clusterClientResponse{Error: err}
				continue
			}

			cc.done = make(chan struct{}, 1)
			go cc.listen(ctx)

			// all fine, return the configuration
			f.responseCh <- clusterClientResponse{Client: cc}

		// or until the context is done
		case <-ctx.Done():
			return
		}
	}
}

type clusterClientResponse struct {
	Client *ClusterClient
	Error  error
}

// ClusterClientConfig contains all configurable parameters
// used when creating a ClusterClient
type ClusterClientConfig struct {
	ConfigPath         string
	VdiskID            string
	StorageClusterName string
}

// NewClusterClient creates a new cluster client
func NewClusterClient(cfg ClusterClientConfig, logger log.Logger) (*ClusterClient, error) {
	if logger == nil {
		logger = log.New(os.Stderr, "", log.LstdFlags)
	}

	cc := &ClusterClient{
		configPath:         cfg.ConfigPath,
		vdiskID:            cfg.VdiskID,
		storageClusterName: cfg.StorageClusterName,
		logger:             logger,
		done:               make(chan struct{}, 1),
	}

	if !cc.loadConfig() {
		return nil, errors.New("couldn't load configuration")
	}

	return cc, nil
}

// ClusterClient contains the cluster configuration,
// which gets reloaded based on incoming SIGHUP signals.
type ClusterClient struct {
	configPath string

	// when storageClusterName is given,
	// vdiskID isn't needed and thus not used
	vdiskID, storageClusterName string

	// used to log
	logger log.Logger

	// keep type, such that we can check this,
	// when reloading the configuration
	vdiskType config.VdiskType

	// used to get a redis connection
	dataConnectionStrings []string
	numberOfServers       int64 //Keep it as a seperate variable since this is constantly needed

	// used as a fallback for getting data from a remote server
	rootDataConnectionString string

	// used to store meta data
	metaConnectionString string

	// indicates if configuration is succesfully loaded
	loaded bool

	// mutex
	mux sync.Mutex

	// used to stop the listening
	done chan struct{}
}

// ConnectionString returns a connectionstring,
// based on a given index, which will be morphed into a local index,
// based on the available (local) storage servers available.
func (cc *ClusterClient) ConnectionString(index int64) (string, error) {
	cc.mux.Lock()
	defer cc.mux.Unlock()

	if !cc.loaded && !cc.loadConfig() {
		return "", errors.New("couldn't load storage cluster config")
	}

	bcIndex := index % cc.numberOfServers
	return cc.dataConnectionStrings[bcIndex], nil
}

// RootConnectionString returns the root connectionstring, if available
func (cc *ClusterClient) RootConnectionString() (string, error) {
	cc.mux.Lock()
	defer cc.mux.Unlock()

	if !cc.loaded && !cc.loadConfig() {
		return "", errors.New("couldn't load storage cluster config")
	}

	return cc.rootDataConnectionString, nil
}

// MetaConnectionString returns the connectionstring (`<host>:<port>`),
// used to connect to the meta storage server.
func (cc *ClusterClient) MetaConnectionString() (string, error) {
	cc.mux.Lock()
	defer cc.mux.Unlock()

	if !cc.loaded && !cc.loadConfig() {
		return "", errors.New("couldn't load storage cluster config")
	}

	return cc.metaConnectionString, nil
}

// Close the open listen goroutine,
// which autoreloads the internal configuration,
// upon receiving a SIGHUP signal.
func (cc *ClusterClient) Close() {
	if cc.done != nil {
		cc.done <- struct{}{}
		close(cc.done)
		cc.done = nil
	}
}

// listen to incoming signals,
// and reload configuration when receiving a SIGHUP signal.
func (cc *ClusterClient) listen(ctx context.Context) {
	cc.logger.Info("ready to reload StorageClusterConfig upon SIGHUP receival for:", cc.vdiskID)

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	defer signal.Stop(ch)

	for {
		select {
		case s := <-ch:
			switch s {
			case syscall.SIGHUP:
				cc.logger.Infof("%q received SIGHUP Signal", cc.vdiskID)
				func() {
					cc.mux.Lock()
					defer cc.mux.Unlock()
					cc.loadConfig()
				}()
			default:
				cc.logger.Info("received unsupported signal", s)
			}

		case <-cc.done:
			cc.logger.Info(
				"exit listener for StorageClusterConfig for vdisk:",
				cc.vdiskID)
			return

		case <-ctx.Done():
			cc.logger.Info(
				"abort listener for StorageClusterConfig for vdisk:",
				cc.vdiskID)
			return
		}
	}
}

func (cc *ClusterClient) loadConfig() bool {
	cc.loaded = false
	cc.logger.Info("loading storage cluster config")

	var storageClusterName string

	cfg, err := config.ReadConfig(cc.configPath)
	if err != nil {
		cc.logger.Infof("couldn't load config at %q: %s", cc.configPath, err)
		return false
	}

	vdisk, ok := cfg.Vdisks[cc.vdiskID]
	if !ok {
		cc.logger.Infof("couldn't find a vdisk %q in the loaded config", cc.vdiskID)
		return false
	}

	// check vdiskType, and sure it's the same one as last time
	if cc.vdiskType != config.VdiskTypeNil && cc.vdiskType != vdisk.Type {
		cc.logger.Infof("wrong type for vdisk %q, expected %q, while received %q",
			cc.vdiskID, cc.vdiskType, vdisk.Type)
		return false
	}
	cc.vdiskType = vdisk.Type

	// remote server, used as fallback for getting data
	cc.rootDataConnectionString = vdisk.RootDataStorage

	if cc.storageClusterName != "" {
		cc.logger.Info("using predefined storage cluster name:", cc.storageClusterName)
		storageClusterName = cc.storageClusterName
	} else {
		// StorageCluster is a /required/ property in the vdisk config,
		// thus this can always be used as a fallback
		storageClusterName = vdisk.Storagecluster
	}

	//Get information about the backend storage nodes
	storageCluster, ok := cfg.StorageClusters[storageClusterName]
	if !ok {
		cc.logger.Infof("couldn't find a storagecluster %s in the loaded config", storageClusterName)
		return false
	}

	// store information required for getting redis connections
	cc.dataConnectionStrings = storageCluster.DataStorage
	cc.numberOfServers = int64(len(cc.dataConnectionStrings))
	if cc.numberOfServers < 1 {
		cc.logger.Info(
			"received no data storage servers, while at least 1 is required")
		return false
	}

	// used to store metadata
	if storageCluster.MetaDataStorage == "" {
		cc.logger.Infof("No metadata servers available in storagecluster %s", storageClusterName)
		return false
	}
	cc.metaConnectionString = storageCluster.MetaDataStorage

	cc.loaded = true
	return cc.loaded
}
