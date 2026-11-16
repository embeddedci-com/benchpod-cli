package serverapi

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// rawBody is a request body doJSON sends as is instead of encoding it as JSON.
type rawBody struct {
	data        []byte
	contentType string
	headers     map[string]string
}

// OTASignatureHeader carries an uploaded image's signed manifest (its .sig file), base64url.
const OTASignatureHeader = "X-BenchPod-Signature"

// OTA job phases (GET /api/benchpod/devices/{id}/ota). Streaming, verifying and committing are
// live; the rest are terminal.
const (
	OTAPhaseStreaming  = "streaming"
	OTAPhaseVerifying  = "verifying"
	OTAPhaseVerified   = "verified" // staged and its hash checked: where a dry run ends
	OTAPhaseCommitting = "committing"
	OTAPhaseDone       = "done"
	OTAPhaseError      = "error"
	OTAPhaseAborted    = "aborted"
)

// OTAJob is a firmware update embeddedci.com runs on a pod over its cloud link.
type OTAJob struct {
	JobID         string `json:"job_id"`
	Phase         string `json:"phase"`
	Live          bool   `json:"live"`
	Percent       int    `json:"percent"`
	SentBytes     int64  `json:"sent_bytes"`
	TotalBytes    int64  `json:"total_bytes"`
	SHA256        string `json:"sha256,omitempty"`
	CommitAfter   bool   `json:"commit_after"`
	Error         string `json:"error,omitempty"`
	VersionBefore string `json:"version_before,omitempty"`
	VersionAfter  string `json:"version_after,omitempty"`
	Release       string `json:"release,omitempty"`
	Step          string `json:"step,omitempty"`
	Part          string `json:"part,omitempty"` // firmware, gw0, gw1, gw2, esp, bridge
	EtaSeconds    int    `json:"eta_seconds,omitempty"`
	Signatures    []struct {
		Part   string `json:"part"`
		Server string `json:"server"` // ok, none, format, unknown-key, signature, target, image
		KeyID  string `json:"key_id,omitempty"`
		Sent   bool   `json:"sent"`
		Device string `json:"device,omitempty"`
	} `json:"signatures,omitempty"`
}

// Terminal reports whether the job has ended (successfully or not).
func (j *OTAJob) Terminal() bool {
	switch j.Phase {
	case OTAPhaseStreaming, OTAPhaseVerifying, OTAPhaseCommitting:
		return false
	}
	return true
}

// OTARequest is what POST /api/benchpod/devices/{id}/ota installs: an uploaded Image (with its
// optional signed manifest), or a published Release by tag, which the server downloads, checks and
// installs with the blobs that go with it.
type OTARequest struct {
	Image     []byte
	Signature []byte // the image's .sig manifest; nil when it has none
	Release   string // a published release tag, in place of Image
	Target    string // "" (firmware), or a blob slot for an uploaded Image: gw0, gw1, gw2, esp, bridge
	DryRun    bool   // stage and verify only (?commit=false)
}

// otaUploadTimeout bounds the upload request: the server reads the whole image (or downloads the
// release) before it answers 202.
const otaUploadTimeout = 5 * time.Minute

// StartOTA calls POST /api/benchpod/devices/{id}/ota. The server answers once the job has started
// (202); follow it with GetOTA. Refusals come back as an *APIError: forbidden_tier (not an
// organization owner or admin), a missing API key scope, lease_held, device_offline, or 409 when
// an update is already running.
func (c *Client) StartOTA(ctx context.Context, credential, deviceID string, req OTARequest) (*OTAJob, error) {
	path, err := otaPath(deviceID, "")
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	var body any
	switch {
	case strings.TrimSpace(req.Release) != "":
		if len(req.Image) > 0 || req.Target != "" {
			return nil, errors.New("serverapi: an OTA installs a release or an uploaded image, not both")
		}
		q.Set("release", strings.TrimSpace(req.Release))
	case len(req.Image) > 0:
		raw := rawBody{data: req.Image, contentType: "application/octet-stream"}
		if len(req.Signature) > 0 {
			raw.headers = map[string]string{OTASignatureHeader: base64.RawURLEncoding.EncodeToString(req.Signature)}
		}
		body = raw
		if req.Target != "" {
			q.Set("target", req.Target)
		}
	default:
		return nil, errors.New("serverapi: an OTA needs an image or a release")
	}
	if req.DryRun {
		q.Set("commit", "false")
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	up := &Client{BaseURL: c.BaseURL, HTTP: &http.Client{Timeout: otaUploadTimeout}, Org: c.Org}
	if c.HTTP != nil && c.HTTP.Transport != nil {
		up.HTTP.Transport = c.HTTP.Transport
	}
	up.orgFor, up.orgResolved = c.cachedOrg()
	var out OTAJob
	if err := up.doAuthedJSON(ctx, http.MethodPost, path, credential, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetOTA calls GET /api/benchpod/devices/{id}/ota: the pod's latest firmware update.
func (c *Client) GetOTA(ctx context.Context, credential, deviceID string) (*OTAJob, error) {
	path, err := otaPath(deviceID, "")
	if err != nil {
		return nil, err
	}
	var out OTAJob
	if err := c.doAuthedJSON(ctx, http.MethodGet, path, credential, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AbortOTA calls POST /api/benchpod/devices/{id}/ota/abort: the pod drops a staged image and the
// job ends aborted. The server refuses once the pod is committing (writing its flash).
func (c *Client) AbortOTA(ctx context.Context, credential, deviceID string) error {
	path, err := otaPath(deviceID, "/abort")
	if err != nil {
		return err
	}
	return c.doAuthedJSON(ctx, http.MethodPost, path, credential, nil, nil)
}

// FirmwareRelease is a published firmware release embeddedci.com can install.
type FirmwareRelease struct {
	Version     string `json:"version"`
	Tag         string `json:"tag"`
	Installable bool   `json:"installable"`
}

// LatestFirmware calls GET /api/benchpod/firmware/latest: the newest published release.
func (c *Client) LatestFirmware(ctx context.Context, credential string) (*FirmwareRelease, error) {
	var out struct {
		Release *FirmwareRelease `json:"release"`
	}
	if err := c.doAuthedJSON(ctx, http.MethodGet, "/api/benchpod/firmware/latest", credential, nil, &out); err != nil {
		return nil, err
	}
	if out.Release == nil || strings.TrimSpace(out.Release.Tag) == "" {
		return nil, errors.New("embeddedci.com has no published firmware release")
	}
	return out.Release, nil
}

func otaPath(deviceID, suffix string) (string, error) {
	id := strings.TrimSpace(deviceID)
	if id == "" {
		return "", errors.New("serverapi: device id is empty")
	}
	return "/api/benchpod/devices/" + url.PathEscape(id) + "/ota" + suffix, nil
}
