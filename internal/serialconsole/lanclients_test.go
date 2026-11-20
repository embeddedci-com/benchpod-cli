package serialconsole

import (
	"errors"
	"strings"
	"testing"
)

const keyA = "3f0c9a1e0000000000000000000000000000000000000000000000000000000a"
const keyB = "9b2e00000000000000000000000000000000000000000000000000000000000b"

func TestParseLanClients(t *testing.T) {
	line := "lan-clients add laptop " + keyA
	raw := "> " + line + "\r\n[net] something\r\nlan-clients added\r\nlan-clients 2 of 32, pairing on\r\n" +
		"  laptop " + keyA + "\r\n  ci-runner-1 " + keyB + "\r\n> "
	l, err := parseLanClients(raw, line)
	if err != nil {
		t.Fatal(err)
	}
	if l.Outcome != "added" || l.Count != 2 || l.Max != 32 || !l.Pairing || len(l.Clients) != 2 {
		t.Fatalf("got %+v", l)
	}
	if l.Clients[1].Name != "ci-runner-1" || !l.Has(strings.ToUpper(keyB)) || l.Has("00") {
		t.Fatalf("clients %+v", l.Clients)
	}
}

func TestParseLanClientsEmptyAndOff(t *testing.T) {
	l, err := parseLanClients("lan-clients\r\nlan-clients 0 of 8, pairing off\r\n> ", "lan-clients")
	if err != nil || l.Count != 0 || l.Max != 8 || l.Pairing || len(l.Clients) != 0 {
		t.Fatalf("got %+v, %v", l, err)
	}
}

func TestParseLanClientsErrors(t *testing.T) {
	_, err := parseLanClients("lan-clients clear\r\nlan-clients error out of memory\r\n", "lan-clients clear")
	var ce *CommandError
	if !errors.As(err, &ce) || ce.Reason != "out of memory" {
		t.Fatalf("error %v", err)
	}
	if _, err := parseLanClients("lan-clients\r\nunknown command: lan-clients\r\n", "lan-clients"); !errors.Is(err, ErrUnknownCommand) {
		t.Fatalf("old firmware: %v", err)
	}
	if _, err := parseLanClients("lan-clients\r\n", "lan-clients"); !errors.Is(err, errNoLanClients) {
		t.Fatalf("no reply: %v", err)
	}
}
