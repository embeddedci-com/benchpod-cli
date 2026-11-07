package serverapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// AivisionDevice is an AI Vision board (BenchPod AI Vision) as embeddedci.com lists it.
type AivisionDevice struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	PublicKey   string     `json:"public_key"`
	Online      bool       `json:"online"`
	ConnectedAt *time.Time `json:"connected_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	// Connection is what the SDK and MCP take: "embeddedci:<name>".
	Connection string `json:"connection"`
}

// RegisterAivisionDevice calls POST /api/aivision/devices: registers the board's key in the
// caller's active organization. Registering the same key again returns the same device.
func (c *Client) RegisterAivisionDevice(ctx context.Context, credential, name, publicKey string) (*AivisionDevice, error) {
	if strings.TrimSpace(name) == "" || strings.TrimSpace(publicKey) == "" {
		return nil, errors.New("serverapi: board name and public key are required")
	}
	var out AivisionDevice
	body := map[string]string{"name": strings.TrimSpace(name), "public_key": strings.TrimSpace(publicKey)}
	if err := c.doAuthedJSON(ctx, http.MethodPost, "/api/aivision/devices", credential, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListAivisionDevices calls GET /api/aivision/devices: the organization's boards.
func (c *Client) ListAivisionDevices(ctx context.Context, credential string) ([]AivisionDevice, error) {
	var out struct {
		Devices []AivisionDevice `json:"devices"`
	}
	if err := c.doAuthedJSON(ctx, http.MethodGet, "/api/aivision/devices", credential, nil, &out); err != nil {
		return nil, err
	}
	return out.Devices, nil
}

// DeregisterAivisionDevice calls POST /api/aivision/devices/{id}/deregister: the board is
// disconnected and its logins are refused until it is registered again.
func (c *Client) DeregisterAivisionDevice(ctx context.Context, credential, deviceID string) (*AivisionDevice, error) {
	id := strings.TrimSpace(deviceID)
	if id == "" {
		return nil, errors.New("serverapi: board id is empty")
	}
	var out AivisionDevice
	if err := c.doAuthedJSON(ctx, http.MethodPost, "/api/aivision/devices/"+url.PathEscape(id)+"/deregister",
		credential, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
