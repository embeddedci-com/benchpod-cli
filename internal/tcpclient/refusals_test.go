package tcpclient

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/fwrefusals"
)

// Every firmware refusal reaches the user word for word: the CLI's hints and retries match on
// these texts, and a user reads them.
func TestFirmwareRefusalsAreShownVerbatim(t *testing.T) {
	for _, r := range fwrefusals.All() {
		if r.Kind != "json" {
			continue
		}
		t.Run(r.ID, func(t *testing.T) {
			reply, _ := json.Marshal(map[string]string{"status": "error", "message": r.Example})
			addr, _ := mockServer(t, []string{string(reply)})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := (&Client{Addr: addr}).Command(ctx, map[string]any{"cmd": "status"})
			if err == nil || err.Error() != r.Example {
				t.Fatalf("err = %v, want %q", err, r.Example)
			}
		})
	}
}
