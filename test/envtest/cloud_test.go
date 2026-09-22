//go:build envtest

package envtest

import (
	"context"
	"fmt"
	"maps"
	"sync"

	"github.com/RiRa12621/machine-api-provider-stackit/pkg/cloud"
)

// localCloud models asynchronous deletion and an accepted create whose response
// is lost. Every access is synchronized because real controllers run concurrently.
type localCloud struct {
	mu                                    sync.Mutex
	server                                *cloud.Server
	volume                                bool
	createCalls, deleteCalls, volumeReads int
	lostResponse                          bool
	beforeCreate                          func(context.Context, cloud.CreateServerInput) error
	journalError                          error
}

func (c *localCloud) factory(context.Context, cloud.Credentials) (cloud.Client, error) { return c, nil }

func cloneServer(server *cloud.Server) *cloud.Server {
	if server == nil {
		return nil
	}
	copy := *server
	copy.Labels = maps.Clone(server.Labels)
	copy.NetworkIDs = append([]string(nil), server.NetworkIDs...)
	copy.Addresses = append([]cloud.Address(nil), server.Addresses...)
	copy.VolumeIDs = append([]string(nil), server.VolumeIDs...)
	if server.BootVolume != nil {
		volume := *server.BootVolume
		copy.BootVolume = &volume
	}
	return &copy
}

func (c *localCloud) GetServer(_ context.Context, id string) (*cloud.Server, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.server == nil || c.server.ID != id {
		return nil, cloud.ErrNotFound
	}
	return cloneServer(c.server), nil
}

func (c *localCloud) ListServers(_ context.Context, labels map[string]string) ([]cloud.Server, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.server == nil {
		return nil, nil
	}
	for key, value := range labels {
		if c.server.Labels[key] != value {
			return nil, nil
		}
	}
	return []cloud.Server{*cloneServer(c.server)}, nil
}

func (c *localCloud) CreateServer(ctx context.Context, input cloud.CreateServerInput) (*cloud.Server, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.createCalls++
	if c.beforeCreate != nil {
		if err := c.beforeCreate(ctx, input); err != nil {
			c.journalError = err
			return nil, err
		}
	}
	if c.server != nil {
		return nil, fmt.Errorf("duplicate cloud create")
	}
	c.server = &cloud.Server{
		ID: "55555555-5555-4555-8555-555555555555", Name: input.Name, Status: "ACTIVE", PowerStatus: "RUNNING",
		MachineType: input.MachineType, AvailabilityZone: input.AvailabilityZone, ImageID: input.ImageID,
		Labels: maps.Clone(input.Labels), NetworkIDs: []string{input.NetworkID}, Addresses: []cloud.Address{{Type: "InternalIP", Address: "10.0.0.12"}},
		BootVolume: &cloud.BootVolume{ID: "66666666-6666-4666-8666-666666666666", DeleteOnTermination: true},
		VolumeIDs:  []string{"66666666-6666-4666-8666-666666666666"},
	}
	c.volume = true
	if c.lostResponse {
		return nil, cloud.ErrTransient
	}
	return cloneServer(c.server), nil
}

func (c *localCloud) DeleteServer(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.server == nil || c.server.ID != id {
		return cloud.ErrNotFound
	}
	c.deleteCalls++
	c.server.Status = "DELETING"
	return nil
}

func (c *localCloud) GetNetwork(_ context.Context, id string) (*cloud.Network, error) {
	return &cloud.Network{ID: id, Status: "CREATED"}, nil
}

func (c *localCloud) GetVolume(_ context.Context, id string) (*cloud.Volume, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.volumeReads++
	if !c.volume {
		return nil, cloud.ErrNotFound
	}
	return &cloud.Volume{ID: id, Status: "DELETING"}, nil
}

func (c *localCloud) counts() (int, int, int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.createCalls, c.deleteCalls, c.volumeReads, c.journalError
}

func (c *localCloud) removeServer() { c.mu.Lock(); defer c.mu.Unlock(); c.server = nil }
func (c *localCloud) removeVolume() { c.mu.Lock(); defer c.mu.Unlock(); c.volume = false }
