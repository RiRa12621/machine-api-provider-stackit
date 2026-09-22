// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/stackitcloud/stackit-sdk-go/core/clients"
	"github.com/stackitcloud/stackit-sdk-go/core/config"
	"github.com/stackitcloud/stackit-sdk-go/core/oapierror"
	iaas "github.com/stackitcloud/stackit-sdk-go/services/iaas/v2api"
)

var (
	uuidPattern   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	labelPattern  = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`)
	regionPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

type sdkClient struct {
	api       *iaas.APIClient
	projectID string
	region    string
}

// NewClient constructs an authenticated client using only the supplied service
// account JSON. Neither environment credentials nor SDK credentials files are
// consulted. Construction does not make cloud API calls.
func NewClient(_ context.Context, creds Credentials) (Client, error) {
	if !uuidPattern.MatchString(creds.ProjectID) || !regionPattern.MatchString(creds.Region) {
		return nil, fmt.Errorf("%w: project UUID and region are required", ErrInvalidInput)
	}
	var key clients.ServiceAccountKeyResponse
	if err := json.Unmarshal(creds.ServiceAccountJSON, &key); err != nil {
		return nil, fmt.Errorf("%w: serviceaccount.json must contain a service account key", ErrInvalidInput)
	}
	if key.Credentials == nil || key.Credentials.PrivateKey == nil ||
		strings.TrimSpace(*key.Credentials.PrivateKey) == "" || key.Credentials.Sub.String() == "00000000-0000-0000-0000-000000000000" ||
		strings.TrimSpace(key.Credentials.Iss) == "" || strings.TrimSpace(key.Credentials.Aud) == "" || strings.TrimSpace(key.Credentials.Kid) == "" {
		return nil, fmt.Errorf("%w: serviceaccount.json must include credentials with privateKey, sub, iss, aud and kid", ErrInvalidInput)
	}
	if endpoint := key.Credentials.TokenEndpoint; endpoint != "" {
		u, err := url.Parse(endpoint)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("%w: credentials tokenEndpoint must be an HTTPS URL", ErrInvalidInput)
		}
	}
	// The SDK's auth.KeyAuth searches ambient private key sources even when a
	// service account key is explicit. Constructing KeyFlow avoids that lookup.
	flow := &clients.KeyFlow{}
	if err := flow.Init(&clients.KeyFlowConfig{
		ServiceAccountKey: &key,
		PrivateKey:        *key.Credentials.PrivateKey,
	}); err != nil {
		return nil, fmt.Errorf("%w: serviceaccount.json contains an invalid private key", ErrInvalidInput)
	}
	api, err := iaas.NewAPIClient(
		config.WithCustomAuth(flow),
		// IaaS v2 uses one global endpoint and a region path parameter. An
		// explicit endpoint also bypasses ambient STACKIT_REGION resolution.
		config.WithEndpoint("https://iaas.api.stackit.cloud"),
		config.WithUserAgent("machine-api-provider-stackit"),
		config.WithHTTPClient(&http.Client{Timeout: 60 * time.Second}),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: unable to configure STACKIT client", ErrInvalidInput)
	}
	return &sdkClient{api: api, projectID: creds.ProjectID, region: creds.Region}, nil
}

func (c *sdkClient) GetServer(ctx context.Context, id string) (*Server, error) {
	if !uuidPattern.MatchString(id) {
		return nil, fmt.Errorf("%w: server ID must be a UUID", ErrInvalidInput)
	}
	server, err := c.api.DefaultAPI.GetServer(ctx, c.projectID, c.region, id).Details(true).Execute()
	if err != nil {
		return nil, classifyError("get server", err)
	}
	if server == nil || server.GetId() != id {
		return nil, fmt.Errorf("%w: get server returned an unexpected identity", ErrTransient)
	}
	return serverFromSDK(server)
}

func (c *sdkClient) ListServers(ctx context.Context, labels map[string]string) ([]Server, error) {
	selector, err := labelSelector(labels)
	if err != nil {
		return nil, err
	}
	response, err := c.api.DefaultAPI.ListServers(ctx, c.projectID, c.region).Details(true).LabelSelector(selector).Execute()
	if err != nil {
		return nil, classifyError("list servers", err)
	}
	if response == nil {
		return nil, fmt.Errorf("%w: list servers returned an empty response", ErrTransient)
	}
	servers := make([]Server, 0, len(response.GetItems()))
	for _, item := range response.GetItems() {
		// Check selectors locally too; cloud filtering alone is not sufficient
		// evidence of ownership. Only string label values can establish a match.
		matches := true
		for key, value := range labels {
			actual, ok := item.GetLabels()[key].(string)
			if !ok || actual != value {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}
		server, err := serverFromSDK(&item)
		if err != nil {
			return nil, err
		}
		servers = append(servers, *server)
	}
	return servers, nil
}

func (c *sdkClient) CreateServer(ctx context.Context, input CreateServerInput) (*Server, error) {
	if input.Name == "" || input.MachineType == "" || !uuidPattern.MatchString(input.ImageID) ||
		!uuidPattern.MatchString(input.NetworkID) || len(input.UserData) == 0 || input.RootVolume.SizeGiB <= 0 {
		return nil, fmt.Errorf("%w: name, machine type, image UUID, network UUID, user data and positive root size are required", ErrInvalidInput)
	}
	if _, err := labelSelector(input.Labels); err != nil {
		return nil, err
	}
	for _, id := range input.SecurityGroups {
		if !uuidPattern.MatchString(id) {
			return nil, fmt.Errorf("%w: security group IDs must be UUIDs", ErrInvalidInput)
		}
	}
	networking := iaas.NewCreateServerNetworking()
	networking.SetNetworkId(input.NetworkID)
	payload := iaas.NewCreateServerPayload(input.MachineType, input.Name, iaas.CreateServerNetworkingAsCreateServerPayloadAllOfNetworking(networking))
	labels := make(map[string]interface{}, len(input.Labels))
	for key, value := range input.Labels {
		labels[key] = value
	}
	payload.SetLabels(labels)
	payload.SetConfigDrive(true)
	payload.SetUserData(base64.StdEncoding.EncodeToString(input.UserData))
	boot := iaas.NewBootVolume()
	boot.SetSource(*iaas.NewBootVolumeSource(input.ImageID, "image"))
	boot.SetSize(input.RootVolume.SizeGiB)
	boot.SetDeleteOnTermination(true)
	if input.RootVolume.PerformanceClass != "" {
		boot.SetPerformanceClass(input.RootVolume.PerformanceClass)
	}
	payload.SetBootVolume(*boot)
	if input.AvailabilityZone != "" {
		payload.SetAvailabilityZone(input.AvailabilityZone)
	}
	if input.SSHKeyName != "" {
		payload.SetKeypairName(input.SSHKeyName)
	}
	if len(input.SecurityGroups) > 0 {
		payload.SetSecurityGroups(input.SecurityGroups)
	}
	server, err := c.api.DefaultAPI.CreateServer(ctx, c.projectID, c.region).CreateServerPayload(*payload).Execute()
	if err != nil {
		return nil, classifyError("create server", err)
	}
	return serverFromSDK(server)
}

func (c *sdkClient) DeleteServer(ctx context.Context, id string) error {
	if !uuidPattern.MatchString(id) {
		return fmt.Errorf("%w: server ID must be a UUID", ErrInvalidInput)
	}
	if err := c.api.DefaultAPI.DeleteServer(ctx, c.projectID, c.region, id).Execute(); err != nil {
		return classifyError("delete server", err)
	}
	return nil
}

func (c *sdkClient) GetNetwork(ctx context.Context, id string) (*Network, error) {
	if !uuidPattern.MatchString(id) {
		return nil, fmt.Errorf("%w: network ID must be a UUID", ErrInvalidInput)
	}
	network, err := c.api.DefaultAPI.GetNetwork(ctx, c.projectID, c.region, id).Execute()
	if err != nil {
		return nil, classifyError("get network", err)
	}
	if network == nil || network.GetId() != id {
		return nil, fmt.Errorf("%w: get network returned an unexpected identity", ErrTransient)
	}
	return &Network{ID: network.GetId(), Status: network.GetStatus()}, nil
}

func (c *sdkClient) GetVolume(ctx context.Context, id string) (*Volume, error) {
	if !uuidPattern.MatchString(id) {
		return nil, fmt.Errorf("%w: volume ID must be a UUID", ErrInvalidInput)
	}
	volume, err := c.api.DefaultAPI.GetVolume(ctx, c.projectID, c.region, id).Execute()
	if err != nil {
		return nil, classifyError("get volume", err)
	}
	if volume == nil || volume.GetId() != id {
		return nil, fmt.Errorf("%w: get volume returned an unexpected identity", ErrTransient)
	}
	return &Volume{ID: volume.GetId(), Status: volume.GetStatus()}, nil
}

func serverFromSDK(in *iaas.Server) (*Server, error) {
	if in == nil || !uuidPattern.MatchString(in.GetId()) {
		return nil, fmt.Errorf("%w: server response has no valid identity", ErrTransient)
	}
	out := &Server{
		ID: in.GetId(), Name: in.GetName(), Status: in.GetStatus(), PowerStatus: in.GetPowerStatus(),
		MachineType: in.GetMachineType(), ImageID: in.GetImageId(), AvailabilityZone: in.GetAvailabilityZone(),
		Labels: make(map[string]string), VolumeIDs: append([]string(nil), in.GetVolumes()...),
	}
	for key, value := range in.GetLabels() {
		if value, ok := value.(string); ok {
			out.Labels[key] = value
		}
	}
	if boot, ok := in.GetBootVolumeOk(); ok {
		out.BootVolume = &BootVolume{ID: boot.GetId(), DeleteOnTermination: boot.GetDeleteOnTermination()}
	}
	for _, nic := range in.GetNics() {
		if id := nic.GetNetworkId(); id != "" {
			out.NetworkIDs = append(out.NetworkIDs, id)
		}
		for _, addr := range []Address{{Type: "InternalIP", Address: nic.GetIpv4()}, {Type: "InternalIP", Address: nic.GetIpv6()}, {Type: "ExternalIP", Address: nic.GetPublicIp()}} {
			if addr.Address == "" {
				continue
			}
			if _, err := netip.ParseAddr(addr.Address); err != nil {
				return nil, fmt.Errorf("%w: server response contains an invalid IP address", ErrTransient)
			}
			out.Addresses = append(out.Addresses, addr)
		}
	}
	return out, nil
}

func labelSelector(labels map[string]string) (string, error) {
	if len(labels) == 0 {
		return "", fmt.Errorf("%w: an ownership label selector is required", ErrInvalidInput)
	}
	parts := make([]string, 0, len(labels))
	for key, value := range labels {
		if !labelPattern.MatchString(key) || strings.HasPrefix(key, "stackit-") || (value != "" && !labelPattern.MatchString(value)) {
			return "", fmt.Errorf("%w: labels must follow STACKIT label syntax", ErrInvalidInput)
		}
		parts = append(parts, key+"="+value)
	}
	sort.Strings(parts)
	return strings.Join(parts, ","), nil
}

// classifyError deliberately excludes response bodies, which can echo
// credentials, bootstrap data, or other sensitive request fields.
func classifyError(operation string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", operation, err)
	}
	var apiError *oapierror.GenericOpenAPIError
	if !errors.As(err, &apiError) {
		return fmt.Errorf("%w: %s request failed", ErrTransient, operation)
	}
	kind := ErrTransient
	switch apiError.StatusCode {
	case http.StatusNotFound:
		kind = ErrNotFound
	case http.StatusUnauthorized, http.StatusForbidden:
		kind = ErrUnauthorized
	case http.StatusConflict:
		kind = ErrConflict
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		kind = ErrInvalidInput
	}
	return fmt.Errorf("%w: %s returned HTTP %d", kind, operation, apiError.StatusCode)
}
