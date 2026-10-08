package serialconsole

import (
	"strings"
	"testing"

	"github.com/embeddedci-com/benchpod-cli/internal/fwrefusals"
)

// The console upload replies the CLI waits for start with a word it recognizes, and the console's
// unknown-command line still says "unknown command" (how the CLI spots old firmware).
func TestConsoleRepliesTheCLIRecognizes(t *testing.T) {
	for _, r := range fwrefusals.All() {
		switch r.Kind {
		case "console_upload":
			cmd, rest, _ := strings.Cut(r.Example, " ")
			word := strings.Fields(rest)[0]
			if !strings.HasPrefix(cmd, "upload-") || !uploadReplyWords[word] {
				t.Errorf("%s: %q: reply word %q is not one awaitReply accepts", r.ID, r.Example, word)
			}
		case "console":
			if !strings.Contains(r.Example, "unknown command") {
				t.Errorf("%s: %q no longer says unknown command", r.ID, r.Example)
			}
		}
	}
}
