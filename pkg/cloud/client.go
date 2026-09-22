// SPDX-License-Identifier: Apache-2.0

// Package cloud provides the STACKIT IaaS operations used by the Machine API
// actuator. It does not manage networks, security groups, or additional volumes.
package cloud

import (
	"context"
	"errors"
)

var (
	ErrNotFound     = errors.New("cloud resource not found")
	ErrInvalidInput = errors.New("invalid cloud input")
	ErrUnauthorized = errors.New("cloud authorization failed")
	ErrConflict     = errors.New("cloud resource conflict")
	ErrTransient    = errors.New("temporary cloud failure")
)

// Credentials must come from the Machine's explicitly referenced Secret.
type Credentials struct {
	ProjectID          string
	Region             string
	ServiceAccountJSON []byte
}

type Factory func(context.Context, Credentials) (Client, error)

// Client is scoped to a single project and region. Callers must validate the
// ownership labels of any resource before adopting or deleting it.
type Client interface {
	GetServer(context.Context, string) (*Server, error)
	ListServers(context.Context, map[string]string) ([]Server, error)
	// CreateServer submits exactly one request. STACKIT does not expose an
	// idempotency token here. Persist creation intent before calling, and recover
	// ambiguous outcomes through ListServers rather than submitting again.
	CreateServer(context.Context, CreateServerInput) (*Server, error)
	// DeleteServer only starts deletion. Callers must observe absence with
	// GetServer before declaring cleanup complete. ErrNotFound means absent.
	DeleteServer(context.Context, string) error
	GetNetwork(context.Context, string) (*Network, error)
	// GetVolume observes root-disk cleanup; it never deletes or detaches disks.
	GetVolume(context.Context, string) (*Volume, error)
}

type Server struct {
	ID               string
	Name             string
	Status           string
	PowerStatus      string
	MachineType      string
	AvailabilityZone string
	ImageID          string
	Labels           map[string]string
	NetworkIDs       []string
	Addresses        []Address
	BootVolume       *BootVolume
	VolumeIDs        []string
}

type Address struct {
	Type    string
	Address string
}

type BootVolume struct {
	ID                  string
	DeleteOnTermination bool
}

type Network struct {
	ID     string
	Status string
}

type Volume struct {
	ID     string
	Status string
}

type CreateServerInput struct {
	Name             string
	ImageID          string
	MachineType      string
	NetworkID        string
	AvailabilityZone string
	SSHKeyName       string
	SecurityGroups   []string
	Labels           map[string]string
	// UserData contains raw Ignition bytes. The adapter base64 encodes them once.
	UserData   []byte
	RootVolume RootVolume
}

// RootVolume is always created from ImageID, never adopted from an existing
// disk. The adapter explicitly enables deletion with the server.
type RootVolume struct {
	SizeGiB          int64
	PerformanceClass string
}
