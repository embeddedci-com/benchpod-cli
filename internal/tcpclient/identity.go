package tcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// IdentityPublic fetches the device's Ed25519 public key via the firmware's
// {"cmd":"identity_public"} command. The returned string is base64url
// (RawURLEncoding, 43 chars decoding to 32 bytes) as emitted by the device.
func (c *Client) IdentityPublic(ctx context.Context) (string, error) {
	raw, err := c.Command(ctx, map[string]any{"cmd": "identity_public"})
	if err != nil {
		return "", err
	}
	var data struct {
		Public string `json:"public"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return "", fmt.Errorf("parse identity_public response: %w", err)
	}
	pub := strings.TrimSpace(data.Public)
	if pub == "" {
		return "", fmt.Errorf("identity_public: device returned an empty public key")
	}
	return pub, nil
}
