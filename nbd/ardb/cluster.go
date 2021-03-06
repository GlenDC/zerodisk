package ardb

import (
	"context"
	"sync"

	"github.com/zero-os/0-Disk/config"
	"github.com/zero-os/0-Disk/errors"
)

// StorageCluster defines the interface of an
// object which allows you to interact with an ARDB Storage Cluster.
type StorageCluster interface {
	// Do applies a given action
	// to the first available server of this cluster.
	Do(action StorageAction) (reply interface{}, err error)
	// DoFor applies a given action
	// to a server within this cluster which maps to the given objectIndex.
	DoFor(objectIndex int64, action StorageAction) (reply interface{}, err error)

	// DoForAll applies each given actions
	// to the servers within this clusters which maps to its paired index.
	// This method should try to group all actions per server if possible,
	// as to avoid too many roundtrips.
	//
	// If an error occurs, only the error will be returned.
	// When no error has occured, it is guaranteed that
	// a reply will be given for each specified pair,
	// as well as the fact that the given replies are returned in
	// the same order that the pairs have been specified.
	DoForAll(pairs []IndexActionPair) (replies []interface{}, err error)

	// ServerIterator returns a server iterator for this Cluster.
	ServerIterator(ctx context.Context) (<-chan StorageServer, error)

	// ServerCount returns the amount of currently available servers within this cluster.
	ServerCount() int64
}

// IndexActionPair pairs an index and a storage action.
type IndexActionPair struct {
	Index  int64
	Action StorageAction
}

// NewUniCluster creates a new (ARDB) uni-cluster.
// See `UniCluster` for more information.
//   ErrNoServersAvailable is returned in case the given server isn't available.
//   ErrServerStateNotSupported is returned in case at the server
// has a state other than StorageServerStateOnline and StorageServerStateRIP.
func NewUniCluster(cfg config.StorageServerConfig, dialer ConnectionDialer) (*UniCluster, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	switch cfg.State {
	case config.StorageServerStateOnline:
		// OK
	case config.StorageServerStateRIP:
		return nil, ErrNoServersAvailable
	default:
		return nil, ErrServerStateNotSupported
	}

	if dialer == nil {
		dialer = stdConnDialer
	}

	return &UniCluster{
		server: cfg,
		dialer: dialer,
	}, nil
}

// UniCluster defines an in memory cluster model for a uni-cluster.
// Meaning it is a cluster with just /one/ ARDB server configured.
// As a consequence all methods will dial connections to this one server.
// This cluster type should only very be used for very specialized purposes.
type UniCluster struct {
	server config.StorageServerConfig
	dialer ConnectionDialer
}

// Do implements StorageCluster.Do
func (cluster *UniCluster) Do(action StorageAction) (interface{}, error) {
	// establish a connection for that serverIndex
	conn, err := cluster.dialer.Dial(cluster.server)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// apply the given action to the established connection
	return action.Do(conn)
}

// DoFor implements StorageCluster.DoFor
func (cluster *UniCluster) DoFor(_ int64, action StorageAction) (interface{}, error) {
	return cluster.Do(action)
}

// DoForAll implements StorageCluster.DoForAll
func (cluster *UniCluster) DoForAll(pairs []IndexActionPair) ([]interface{}, error) {
	switch n := len(pairs); n {
	case 0:
		return nil, nil

	case 1:
		reply, err := cluster.DoFor(pairs[0].Index, pairs[0].Action)
		if err != nil {
			return nil, err
		}
		return []interface{}{reply}, nil

	default:
		actions := make([]StorageAction, n)
		for index, pair := range pairs {
			actions[index] = pair.Action
		}
		return Values(cluster.Do(Commands(actions...)))
	}
}

// ServerIterator implements StorageCluster.ServerIterator
func (cluster *UniCluster) ServerIterator(context.Context) (<-chan StorageServer, error) {
	ch := make(chan StorageServer, 1)
	server := storageServer{
		cfg:    cluster.server,
		dialer: cluster.dialer,
	}
	ch <- server
	close(ch)
	return ch, nil
}

// ServerCount implements StorageCluster.ServerCount
func (cluster *UniCluster) ServerCount() int64 {
	return 1
}

// NewCluster creates a new (ARDB) cluster.
// ErrNoServersAvailable is returned in case no given server is available.
// ErrServerStateNotSupported is returned in case at least one server
// has a state other than StorageServerStateOnline and StorageServerStateRIP.
func NewCluster(cfg config.StorageClusterConfig, dialer ConnectionDialer) (*Cluster, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	serverCount := int64(len(cfg.Servers))
	availableServerCount := serverCount

	for _, server := range cfg.Servers {
		if server.State == config.StorageServerStateOnline {
			continue
		}
		if server.State != config.StorageServerStateRIP {
			return nil, ErrServerStateNotSupported
		}
		availableServerCount--
	}
	if availableServerCount == 0 {
		return nil, ErrNoServersAvailable
	}

	if dialer == nil {
		dialer = stdConnDialer
	}

	return &Cluster{
		servers:              cfg.Servers,
		serverCount:          serverCount,
		availableServerCount: availableServerCount,
		dialer:               dialer,
	}, nil
}

// Cluster defines the in memory cluster model for a single cluster.
type Cluster struct {
	servers              []config.StorageServerConfig
	serverCount          int64
	availableServerCount int64

	dialer ConnectionDialer
}

// Do implements StorageCluster.Do
func (cluster *Cluster) Do(action StorageAction) (interface{}, error) {
	// compute server index of first available server
	serverIndex, err := FindFirstServerIndex(cluster.serverCount, cluster.serverOperational)
	if err != nil {
		return nil, err
	}
	return cluster.doAt(serverIndex, action)
}

// DoFor implements StorageCluster.DoFor
func (cluster *Cluster) DoFor(objectIndex int64, action StorageAction) (interface{}, error) {
	// compute server index which maps to the given object index
	serverIndex, err := ComputeServerIndex(cluster.serverCount, objectIndex, cluster.serverOperational)
	if err != nil {
		return nil, err
	}
	return cluster.doAt(serverIndex, action)
}

// IndexActionMap is a helper structure,
// which can be used to link some index to some action,
// instead of a map, as a map has no stable order.
//
// This structure was designed for and is used to help implement (*Cluster).DoForAll,
// to be able to collect and return all replies in ordered form,
// as that has to be guaranteed for all (StorageCluster).DoForAll implementations.
type IndexActionMap struct {
	Indices []int64
	Actions []StorageAction
}

// Add an index and action to a map,
// this is just a helper method which appends both the index and the action
// to the their relative slice type.
func (m *IndexActionMap) Add(index int64, action StorageAction) {
	m.Indices = append(m.Indices, index)
	m.Actions = append(m.Actions, action)
}

// DoForAll implements StorageCluster.DoForAll
func (cluster *Cluster) DoForAll(pairs []IndexActionPair) ([]interface{}, error) {
	// a shortcut in case we have received no pairs, or just a single one
	switch len(pairs) {
	case 0:
		return nil, nil // nothing to do
	case 1:
		reply, err := cluster.DoFor(pairs[0].Index, pairs[1].Action)
		if err != nil {
			return nil, err
		}
		return []interface{}{reply}, nil
	}

	// sort all actions in terms of their mapped server index
	servers := make(map[int64]*IndexActionMap)
	for index, pair := range pairs {
		// compute server index which maps to the given object index
		serverIndex, err := ComputeServerIndex(cluster.serverCount, pair.Index, cluster.serverOperational)
		if err != nil {
			return nil, err
		}
		// add the pair to the relevant map
		server, ok := servers[serverIndex]
		if !ok {
			server = new(IndexActionMap)
			servers[serverIndex] = server
		}
		server.Add(int64(index), pair.Action)
	}

	// a shortcut if we are lucky enough to only have 1 server
	if len(servers) == 1 {
		for serverIndex, indexActionMap := range servers {
			return Values(cluster.doAt(serverIndex, Commands(indexActionMap.Actions...)))
		}
	}

	// create result -type and -channel to collect all replies async
	type serverResult struct {
		ServerIndex int64
		Replies     []interface{}
		Error       error
	}
	ch := make(chan serverResult)

	// apply all actions async, and collect the results

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	for serverIndex := range servers {
		wg.Add(1)

		// local variables to be used within the goroutine scope
		indexActionMap := servers[serverIndex]
		result := serverResult{ServerIndex: serverIndex}

		go func() {
			defer wg.Done()
			result.Replies, result.Error = Values(cluster.doAt(
				result.ServerIndex, Commands(indexActionMap.Actions...)))
			select {
			case ch <- result:
			case <-ctx.Done():
			}
		}()
	}

	// close channel when all inpu
	go func() {
		wg.Wait()
		close(ch)
	}()

	// we'll collect all replies in order
	replies := make([]interface{}, len(pairs))

	// collect all replies
	for result := range ch {
		// return early if an error occured
		if result.Error != nil {
			return nil, errors.Wrapf(result.Error,
				"error while applying actions in serverIndex %d", result.ServerIndex)
		}

		// get the indexActionMap for the given server,
		// such that we can retrieve the correct reply index
		m := servers[result.ServerIndex]

		// collect all received replies in order
		for i, reply := range result.Replies {
			replyIndex := m.Indices[i]
			replies[replyIndex] = reply
		}
	}

	// return all replies from all servers in ordered form
	return replies, nil
}

// ServerIterator implements StorageCluster.ServerIterator
func (cluster *Cluster) ServerIterator(ctx context.Context) (<-chan StorageServer, error) {
	ch := make(chan StorageServer)
	go func() {
		defer close(ch)

		for _, cfg := range cluster.servers {
			if cfg.State != config.StorageServerStateOnline {
				continue
			}

			server := storageServer{
				cfg:    cfg,
				dialer: cluster.dialer,
			}

			select {
			case <-ctx.Done():
				return
			case ch <- server:
			}
		}
	}()
	return ch, nil
}

// ServerCount implements StorageCluster.ServerCount
func (cluster *Cluster) ServerCount() int64 {
	return cluster.availableServerCount
}

func (cluster *Cluster) doAt(serverIndex int64, action StorageAction) (interface{}, error) {
	// establish a connection for that serverIndex
	conn, err := cluster.dialer.Dial(cluster.servers[serverIndex])
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// apply the given action to the established connection
	return action.Do(conn)
}

// serverOperational returns true if
// the server on the given index is operational.
func (cluster *Cluster) serverOperational(index int64) (bool, error) {
	// Because of the constructor,
	// we know for sure that the state is either online or RIP.
	ok := cluster.servers[index].State == config.StorageServerStateOnline
	return ok, nil
}

// storageServer is the std storage server.
type storageServer struct {
	cfg    config.StorageServerConfig
	dialer ConnectionDialer
}

// Do implements StorageServer.Do
func (server storageServer) Do(action StorageAction) (reply interface{}, err error) {
	// establish a connection for that serverIndex
	conn, err := server.dialer.Dial(server.cfg)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// apply the given action to the established connection
	return action.Do(conn)
}

// Config implements StorageServer.Config
func (server storageServer) Config() config.StorageServerConfig {
	return server.cfg
}

// ErrorCluster is a Cluster which can be used for
// scenarios where you want to specify a StorageCluster,
// which only ever returns a static error.
type ErrorCluster struct {
	Error error
}

// Do implements StorageCluster.Do
func (cluster ErrorCluster) Do(action StorageAction) (reply interface{}, err error) {
	return nil, cluster.Error
}

// DoFor implements StorageCluster.DoFor
func (cluster ErrorCluster) DoFor(objectIndex int64, action StorageAction) (reply interface{}, err error) {
	return nil, cluster.Error
}

// DoForAll implements StorageCluster.DoForAll
func (cluster ErrorCluster) DoForAll(pairs []IndexActionPair) (replies []interface{}, err error) {
	return nil, cluster.Error
}

// ServerIterator implements StorageCluster.ServerIterator
func (cluster ErrorCluster) ServerIterator(context.Context) (<-chan StorageServer, error) {
	ch := make(chan StorageServer, 1)
	ch <- errorStorageServer{Error: cluster.Error}
	close(ch)
	return ch, nil
}

// ServerCount implements StorageCluster.ServerCount
func (cluster ErrorCluster) ServerCount() int64 {
	return 0
}

// errorStorageServer is a server which can be used for
// scenarios where you want to specify a StorageServer,
// which only ever returns a static error.
type errorStorageServer struct {
	Error error
}

// Do implements StorageServer.Do
func (server errorStorageServer) Do(action StorageAction) (reply interface{}, err error) {
	return nil, server.Error
}

// Config implements StorageServer.Config
func (server errorStorageServer) Config() config.StorageServerConfig {
	return config.StorageServerConfig{}
}

// NopCluster is a Cluster which can be used for
// scenarios where you want to specify a StorageCluster,
// which returns no errors and no content.
type NopCluster struct{}

// Do implements StorageCluster.Do
func (cluster NopCluster) Do(_ StorageAction) (reply interface{}, err error) {
	return nil, nil
}

// DoFor implements StorageCluster.DoFor
func (cluster NopCluster) DoFor(objectIndex int64, action StorageAction) (reply interface{}, err error) {
	return nil, nil
}

// DoForAll implements StorageCluster.DoForAll
func (cluster NopCluster) DoForAll(pairs []IndexActionPair) (replies []interface{}, err error) {
	return nil, nil
}

// ServerIterator implements StorageCluster.ServerIterator
func (cluster NopCluster) ServerIterator(context.Context) (<-chan StorageServer, error) {
	ch := make(chan StorageServer, 1)
	ch <- nopStorageServer{}
	close(ch)
	return ch, nil
}

// ServerCount implements StorageCluster.ServerCount
func (cluster NopCluster) ServerCount() int64 {
	return 1
}

// nopStorageServer is a server which can be used for
// scenarios where you want to specify a StorageServer,
// which returns no errors and no content.
type nopStorageServer struct{}

// Do implements StorageServer.Do
func (server nopStorageServer) Do(action StorageAction) (reply interface{}, err error) {
	return nil, nil
}

// Config implements StorageServer.Config
func (server nopStorageServer) Config() config.StorageServerConfig {
	return config.StorageServerConfig{}
}

// StorageServer defines the interface of an
// object which allows you to interact with an ARDB Storage Server.
type StorageServer interface {
	// Do applies a given action to this storage server.
	Do(action StorageAction) (reply interface{}, err error)

	// Config returns the storage server config used for this storage server.
	Config() config.StorageServerConfig
}

// ServerIndexPredicate is a predicate
// used to determine if a given serverIndex
// for a callee-owned cluster object is valid.
// An error can be returned to quit early in a search.
type ServerIndexPredicate func(serverIndex int64) (bool, error)

// FindFirstAvailableServerConfig iterates through all storage servers
// until it finds a server which state indicates its available.
// If no such server exists, ErrNoServersAvailable is returned.
// TODO: delete this function,
//       and instead force primitives to work with clusters,
//       as otherwise we might start to diviate in decision-making,
//       depending on whether this funcion is used or the static cluster type.
// see: https://github.com/zero-os/0-Disk/issues/549
func FindFirstAvailableServerConfig(cfg config.StorageClusterConfig) (serverCfg config.StorageServerConfig, err error) {
	for _, serverCfg = range cfg.Servers {
		if serverCfg.State == config.StorageServerStateOnline {
			return
		}
	}

	err = ErrNoServersAvailable
	return
}

// FindFirstServerIndex iterates through all servers
// until the predicate for a server index returns true.
// If no index evaluates to true, ErrNoServersAvailable is returned.
func FindFirstServerIndex(serverCount int64, predicate ServerIndexPredicate) (serverIndex int64, err error) {
	var ok bool
	for ; serverIndex < serverCount; serverIndex++ {
		ok, err = predicate(serverIndex)
		if ok || err != nil {
			return
		}
	}

	return -1, ErrNoServersAvailable
}

// ComputeServerIndex computes a server index for a given objectIndex,
// using a shared static algorithm with the serverCount as input and
// a given predicate to define if a computed index is fine.
// NOTE: the callee has to ensure that at least /one/ server is operational!
func ComputeServerIndex(serverCount, objectIndex int64, predicate ServerIndexPredicate) (serverIndex int64, err error) {
	// first try the modulo sharding,
	// which will work for all default online shards
	// and thus keep it as cheap as possible
	serverIndex = objectIndex % serverCount
	ok, err := predicate(serverIndex)
	if ok || err != nil {
		return
	}

	// keep trying until we find an online server
	// in the same reproducable manner
	// (another kind of tracing)
	// using jumpConsistentHash taken from https://arxiv.org/pdf/1406.2294.pdf
	var j int64
	var key uint64
	for {
		key = uint64(objectIndex)
		j = 0
		for j < serverCount {
			serverIndex = j
			key = key*2862933555777941757 + 1
			j = int64(float64(serverIndex+1) * (float64(int64(1)<<31) / float64((key>>33)+1)))
		}
		ok, err = predicate(serverIndex)
		if ok || err != nil {
			return
		}

		objectIndex++
	}
}

var (
	stdConnDialer = new(StandardConnectionDialer)
)

// enforces that our std StorageClusters
// are actually StorageClusters
var (
	_ StorageCluster = (*UniCluster)(nil)
	_ StorageCluster = (*Cluster)(nil)
	_ StorageCluster = NopCluster{}
	_ StorageCluster = ErrorCluster{}
)

// enforces that our std StorageServers
// are actually StorageServers
var (
	_ StorageServer = storageServer{}
	_ StorageServer = errorStorageServer{}
	_ StorageServer = nopStorageServer{}
)

var (
	// ErrServerUnavailable is returned in case
	// a given server is unavailable (e.g. offline).
	ErrServerUnavailable = errors.New("server is unavailable")

	// ErrNoServersAvailable is returned in case
	// no servers are available for usage.
	ErrNoServersAvailable = errors.New("no servers available")

	// ErrServerIndexOOB is returned in case
	// a given server index used is out of bounds,
	// for the cluster context it is used within.
	ErrServerIndexOOB = errors.New("server index out of bounds")

	// ErrInvalidInput is an error returned
	// when the input given for a function is invalid.
	ErrInvalidInput = errors.New("invalid input given")

	// ErrServerStateNotSupported is an error ereturned
	// when a server is updated to a state,
	// while the cluster (type) does not support that kind of state.
	ErrServerStateNotSupported = errors.New("server state is not supported")
)
