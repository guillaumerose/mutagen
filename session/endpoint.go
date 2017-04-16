package session

import (
	"context"
	"hash"
	"io"
	"time"

	"github.com/pkg/errors"

	"github.com/havoc-io/mutagen/encoding"
	"github.com/havoc-io/mutagen/filesystem"
	"github.com/havoc-io/mutagen/message"
	"github.com/havoc-io/mutagen/multiplex"
	"github.com/havoc-io/mutagen/rsync"
	"github.com/havoc-io/mutagen/sync"
)

// endpoint encodes and coordinates endpoint state between multiple servers. It
// doesn't have a constructor, but is built and run inside ServeEndpoint.
type endpoint struct {
	// root is the synchronization root for the endpoint. It is static.
	root string
	// ignores is the list of ignored paths for the session. It is static.
	ignores []string
	// cachePath is the path at which to save the cache for the session. It is
	// static.
	cachePath string
	// cache is the cache from the last successful scan on the endpoint. It is
	// owned by the serveControl Goroutine.
	cache *sync.Cache
	// scanRsyncEngine is the rsync engine used to compute snapshot deltas. It
	// is owned by the serveControl Goroutine.
	scanEngine *rsync.Engine
	// scanHasher is the hasher used for scans. It is owned by the serveControl
	// Goroutine.
	scanHasher hash.Hash
	// stagingCoordinator is the staging coordinator. It is owned by the
	// serveControl Goroutine.
	stagingCoordinator *stagingCoordinator
	// stagingClient is the rsync client for staging files. It is owned by the
	// serveControl Goroutine.
	stagingClient *rsync.Client
}

// TODO: Document that this function relies on the connection unblocking reads
// and writes when closed.
func ServeEndpoint(connection io.ReadWriteCloser) error {
	// Perform housekeeping.
	housekeep()

	// Ensure that the connection is closed when we're done.
	defer connection.Close()

	// Perform multiplexing and ensure the multiplexer is shut down when we're
	// done.
	streams, multiplexer := multiplex.ReadWriter(connection, numberOfEndpointChannels)
	defer multiplexer.Close()

	// Create a cancellable context with which to terminate Goroutines that we
	// create and ensure that it's cancelled when we're done. This only applies
	// to Goroutines that block in channels - all other Goroutines are cancelled
	// by closing the underlying network connection.
	serveContext, cancelServe := context.WithCancel(context.Background())
	defer cancelServe()

	// Convert the control channel to a message stream.
	control := message.NewMessageStream(streams[endpointChannelControl])

	// Receive the initialization request.
	var init initializeRequest
	if err := control.Decode(&init); err != nil {
		return errors.Wrap(err, "unable to receive initialization request")
	}

	// Validate the initialization request.
	if init.Session == "" {
		return errors.New("empty session identifier")
	} else if !init.Version.supported() {
		return errors.New("unsupported session version")
	} else if init.Root == "" {
		return errors.New("empty root path")
	}

	// Expand and normalize the root path.
	root, err := filesystem.Normalize(init.Root)
	if err != nil {
		return errors.Wrap(err, "unable to normalize root path")
	}

	// Compute the cache path.
	cachePath, err := pathForCache(init.Session, init.Alpha)
	if err != nil {
		return errors.Wrap(err, "unable to compute/create cache path")
	}

	// Load any existing cache. If it fails, just replace it with an empty one.
	cache := &sync.Cache{}
	if encoding.LoadAndUnmarshalProtobuf(cachePath, cache) != nil {
		cache = &sync.Cache{}
	}

	// Create a staging coordinator.
	stagingCoordinator, err := newStagingCoordinator(init.Session, init.Version, init.Alpha)
	if err != nil {
		return errors.Wrap(err, "unable to create staging coordinator")
	}

	// Create the rsync client and ensure that all polling on its state is
	// terminated when we're done.
	stagingClient := rsync.NewClient(
		streams[endpointChannelRsyncClient],
		root,
		stagingCoordinator,
	)
	defer stagingClient.CancelAllStatePollers()

	// Send the initialization response.
	initResponse := initializeResponse{
		PreservesExecutability: filesystem.PreservesExecutability,
	}
	if err = control.Encode(initResponse); err != nil {
		return errors.Wrap(err, "unable to send initialization response")
	}

	// Create the endpoint.
	endpoint := &endpoint{
		root:               root,
		ignores:            init.Ignores,
		cachePath:          cachePath,
		cache:              cache,
		scanEngine:         rsync.NewEngine(),
		scanHasher:         init.Version.hasher(),
		stagingCoordinator: stagingCoordinator,
		stagingClient:      stagingClient,
	}

	// Start serving rsync requests and monitor for failure.
	serveRsyncErrors := make(chan error, 1)
	go func() {
		serveRsyncErrors <- endpoint.serveRsync(streams[endpointChannelRsyncServer])
	}()

	// Start serving watch events and monitor for failure.
	serveWatchErrors := make(chan error, 1)
	go func() {
		serveWatchErrors <- endpoint.serveWatch(serveContext, streams[endpointChannelWatchEvents])
	}()

	// Start serving rsync state updates.
	transmitRsyncClientStateErrors := make(chan error, 1)
	go func() {
		transmitRsyncClientStateErrors <- endpoint.transmitRsyncClientState(streams[endpointChannelRsyncUpdates])
	}()

	// Start serving control requests.
	serveControlErrors := make(chan error, 1)
	go func() {
		serveControlErrors <- endpoint.serveControl(control)
	}()

	// Wait for any of the serving components to fail.
	select {
	case err = <-serveRsyncErrors:
		return errors.Wrap(err, "rsync server failure")
	case err = <-serveWatchErrors:
		return errors.Wrap(err, "watch server failure")
	case err = <-transmitRsyncClientStateErrors:
		return errors.Wrap(err, "rsync state transmission failure")
	case err = <-serveControlErrors:
		return errors.Wrap(err, "control server failure")
	}
}

func (e *endpoint) serveRsync(connection io.ReadWriter) error {
	return rsync.Serve(connection, e.root)
}

func (e *endpoint) serveWatch(context context.Context, connection io.ReadWriter) error {
	// Convert the connection to a message stream.
	stream := message.NewMessageStream(connection)

	// TODO: Implement using watching or scanning. For now, we just use a timer.
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := stream.Encode(struct{}{}); err != nil {
				return errors.Wrap(err, "unable to transmit watch event")
			}
		case <-context.Done():
			return errors.New("cancelled")
		}
	}
}

func (e *endpoint) transmitRsyncClientState(connection io.ReadWriter) error {
	// Convert the connection to a message stream.
	stream := message.NewMessageStream(connection)

	// Loop on client state changes until there's an error.
	var state rsync.StagingStatus
	var stateIndex uint64
	var err error
	for {
		// Poll for the next client state change.
		state, stateIndex, err = e.stagingClient.State(stateIndex)
		if err != nil {
			return errors.Wrap(err, "unable to poll client state")
		}

		// Transmit the client state.
		if err = stream.Encode(state); err != nil {
			return errors.Wrap(err, "unable to transmit state")
		}
	}
}

func (e *endpoint) serveControl(stream message.MessageStream) error {
	// Receive and process control requests until there's an error.
	for {
		// Grab the next request.
		var request endpointRequest
		if err := stream.Decode(&request); err != nil {
			return errors.Wrap(err, "unable to decode request")
		}

		// Dispatch the request accordingly.
		if request.Scan != nil {
			if response, err := e.handleScan(request.Scan); err != nil {
				return errors.Wrap(err, "unable to perform scan")
			} else if err = stream.Encode(response); err != nil {
				return errors.Wrap(err, "unable to send scan response")
			}
		} else if request.Stage != nil {
			if response, err := e.handleStage(request.Stage); err != nil {
				return errors.Wrap(err, "unable to perform staging")
			} else if err = stream.Encode(response); err != nil {
				return errors.Wrap(err, "unable to send stage response")
			}
		} else if request.Transition != nil {
			if err := stream.Encode(e.handleTransition(request.Transition)); err != nil {
				return errors.Wrap(err, "unable to send transition response")
			}
		} else {
			return errors.New("invalid request")
		}
	}
}

func (e *endpoint) handleScan(request *scanRequest) (*scanResponse, error) {
	// Create a snapshot. If this fails, we have to consider the possibility
	// that it's due to concurrent modifications. In that case, we just suggest
	// that the controller re-try later.
	snapshot, cache, err := sync.Scan(e.root, e.scanHasher, e.cache, e.ignores)
	if err != nil {
		return &scanResponse{TryAgain: true}, nil
	}

	// Store the cache.
	if err = encoding.MarshalAndSaveProtobuf(e.cachePath, cache); err != nil {
		return nil, errors.Wrap(err, "unable to save cache")
	}
	e.cache = cache

	// Marshal the snapshot.
	snapshotBytes, err := marshalEntry(snapshot)
	if err != nil {
		return nil, errors.Wrap(err, "unable to marshal snapshot")
	}

	// Compute it's delta against the base.
	delta := e.scanEngine.DeltafyBytes(snapshotBytes, request.BaseSnapshotSignature)

	// Success.
	return &scanResponse{SnapshotDelta: delta}, nil
}

func (e *endpoint) handleStage(request *stageRequest) (*stageResponse, error) {
	// Compute the paths that need to be staged.
	paths, err := stagingPathsForChanges(request.Transitions)
	if err != nil {
		return nil, errors.Wrap(err, "unable to extract staging paths")
	}

	// Perform staging.
	if err = e.stagingClient.Stage(paths); err != nil {
		return nil, errors.Wrap(err, "unable to stage files")
	}

	// Success.
	return &stageResponse{}, nil
}

func (e *endpoint) handleTransition(request *transitionRequest) *transitionResponse {
	// Perform the transition.
	changes, problems := sync.Transition(
		e.root,
		request.Transitions,
		e.cache,
		e.stagingCoordinator,
	)

	// Wipe the staging directory. We don't monitor for errors here, because we
	// need to return the changes and problems no matter what, but if there's
	// something weird going on with the filesystem, we'll see it the next time
	// we scan or stage.
	e.stagingCoordinator.wipe()

	// Done.
	return &transitionResponse{changes, problems}
}
