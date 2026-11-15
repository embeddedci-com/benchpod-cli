package tcpclient

import (
	"context"
	"encoding/json"
	"fmt"
)

// StagePSRAM uploads data into the pod's PSRAM staging area (`load_bin` with "psram":true) on a
// connection of its own, without arming anything: a following command (`esp_flash`, `spi_stream`)
// uses it. It returns the bytes the pod stored. The pod acknowledges progress with {"ack":N} lines
// while it receives; those are skipped until the final status line.
func (c *Client) StagePSRAM(ctx context.Context, data []byte) (int, error) {
	if len(data) == 0 {
		return 0, fmt.Errorf("load_bin: nothing to upload")
	}
	conn, reader, err := c.dialAndSend(ctx, map[string]any{"cmd": "load_bin", "total": len(data), "psram": true})
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	defer watchContext(ctx, conn)()

	r, err := readReply(reader)
	if err != nil {
		return 0, readErr(ctx, err)
	}
	if r.Status == "error" {
		return 0, podError(r.Message)
	}
	if r.Status != "ok" {
		return 0, fmt.Errorf("load_bin: unexpected response status %q", r.Status)
	}
	if _, err := conn.Write(data); err != nil {
		return 0, readErr(ctx, fmt.Errorf("load_bin: send: %w", err))
	}
	for {
		r, err := readReply(reader)
		if err != nil {
			return 0, readErr(ctx, err)
		}
		switch r.Status {
		case "": // {"ack":N}: progress, keep reading
			continue
		case "error":
			return 0, podError(r.Message)
		case "ok":
			var d struct {
				Total *int `json:"total"`
			}
			if len(r.Data) > 0 && json.Unmarshal(r.Data, &d) == nil && d.Total != nil {
				return *d.Total, nil
			}
			return len(data), nil
		default:
			return 0, fmt.Errorf("load_bin: unexpected response status %q", r.Status)
		}
	}
}
