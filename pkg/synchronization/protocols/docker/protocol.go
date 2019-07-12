package docker

import (
	"github.com/pkg/errors"

	"github.com/havoc-io/mutagen/pkg/agent"
	"github.com/havoc-io/mutagen/pkg/agent/transports/docker"
	"github.com/havoc-io/mutagen/pkg/synchronization"
	"github.com/havoc-io/mutagen/pkg/synchronization/endpoint/remote"
	urlpkg "github.com/havoc-io/mutagen/pkg/url"
)

// protocolHandler implements the session.ProtocolHandler interface for
// connecting to remote endpoints inside Docker containers. It uses the agent
// infrastructure over a Docker transport.
type protocolHandler struct{}

// Connect connects to a Docker endpoint.
func (h *protocolHandler) Connect(
	url *urlpkg.URL,
	prompter string,
	session string,
	version synchronization.Version,
	configuration *synchronization.Configuration,
	alpha bool,
) (synchronization.Endpoint, error) {
	// Verify that the URL is of the correct kind and protocol.
	if url.Kind != urlpkg.Kind_Synchronization {
		panic("non-synchronization URL dispatched to synchronization protocol handler")
	} else if url.Protocol != urlpkg.Protocol_Docker {
		panic("non-Docker URL dispatched to Docker protocol handler")
	}

	// Create a Docker agent transport.
	transport, err := docker.NewTransport(url.Host, url.User, url.Environment, prompter)
	if err != nil {
		return nil, errors.Wrap(err, "unable to create Docker transport")
	}

	// Dial an agent in endpoint mode.
	connection, err := agent.Dial(transport, agent.ModeEndpoint, prompter)
	if err != nil {
		return nil, errors.Wrap(err, "unable to dial agent endpoint")
	}

	// Create the endpoint client.
	return remote.NewEndpointClient(connection, url.Path, session, version, configuration, alpha)
}

func init() {
	// Register the Docker protocol handler with the synchronization package.
	synchronization.ProtocolHandlers[urlpkg.Protocol_Docker] = &protocolHandler{}
}
