package serialconsole

import (
	"strings"

	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// ErrUnknownCommand is older firmware's answer to a console command it does not have. It is the
// same value as ErrPolicyUnsupported, so either name matches with errors.Is.
var ErrUnknownCommand = ErrPolicyUnsupported

// CommandError is the pod refusing a console command ("<cmd> error <why>"). It is the CLI's one
// refusal type, tcpclient.PodError, with Cmd set.
type CommandError = tcpclient.PodError

// consoleLines splits raw console output into trimmed lines, dropping prompts and backspaces.
func consoleLines(raw string) []string {
	s := strings.ReplaceAll(raw, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	var lines []string
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(strings.TrimLeft(ln, "> \t\x08"))
		if ln != "" {
			lines = append(lines, ln)
		}
	}
	return lines
}

// lastReply returns what follows "<word> " on the last line that starts with it, skipping the
// first copy of the echoed command line.
func lastReply(raw, word, line string) (string, bool) {
	echoSeen := false
	var rest string
	found := false
	for _, ln := range consoleLines(raw) {
		if !echoSeen && ln == line {
			echoSeen = true
			continue
		}
		if strings.HasPrefix(ln, word+" ") {
			rest, found = strings.TrimSpace(ln[len(word)+1:]), true
		}
	}
	return rest, found && rest != ""
}

// replyError reports whether a reply is "error <why>", and the why.
func replyError(rest string) (string, bool) {
	if rest != "error" && !strings.HasPrefix(rest, "error ") {
		return "", false
	}
	why := strings.TrimSpace(strings.TrimPrefix(rest, "error"))
	if why == "" {
		why = "refused"
	}
	return why, true
}
