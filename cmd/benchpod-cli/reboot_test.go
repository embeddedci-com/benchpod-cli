package main

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// rebootPod is a pod whose reboot command is answered by reply ("" drops the connection without
// a reply, as a pod that restarts before the reply flushes). Status answers once the pod is "up".
type rebootPod struct {
	mu    sync.Mutex
	reqs  []string
	up    bool // status answers
	delay int  // status refusals (connection drops) before it is up again
}

func startRebootPod(t *testing.T, reply string, statusDownFor int) (string, *rebootPod) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	p := &rebootPod{delay: statusDownFor}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				line, _ := bufio.NewReader(conn).ReadString('\n')
				p.mu.Lock()
				p.reqs = append(p.reqs, strings.TrimSpace(line))
				p.mu.Unlock()
				switch {
				case strings.Contains(line, `"cmd":"reboot"`):
					if reply != "" {
						_, _ = io.WriteString(conn, reply+"\n")
					}
				case strings.Contains(line, `"cmd":"status"`):
					p.mu.Lock()
					down := p.delay > 0
					if down {
						p.delay--
					}
					p.mu.Unlock()
					if !down {
						_, _ = io.WriteString(conn, `{"status":"ok","data":{"version":"4.0.7","api":2,"min_client_api":2}}`+"\n")
					}
				}
			}()
		}
	}()
	return ln.Addr().String(), p
}

func (p *rebootPod) requests() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.reqs...)
}

func fastReboot(t *testing.T) {
	t.Helper()
	s, w, r := rebootSettle, rebootWait, rebootRetry
	rebootSettle, rebootWait, rebootRetry = 0, 3*time.Second, 20*time.Millisecond
	t.Cleanup(func() { rebootSettle, rebootWait, rebootRetry = s, w, r })
}

func TestRebootSoftReply(t *testing.T) {
	fastReboot(t)
	addr, pod := startRebootPod(t, `{"status":"ok","data":{"reboot":"soft"}}`, 2)
	var out bytes.Buffer
	if err := runReboot(&globalFlags{connection: addr}, "soft", true, &out); err != nil {
		t.Fatal(err)
	}
	want := "Pod at " + addr + " rebooted (soft), firmware 4.0.7.\n"
	if out.String() != want {
		t.Fatalf("output %q, want %q", out.String(), want)
	}
	if reqs := pod.requests(); !strings.Contains(reqs[0], `"cmd":"reboot"`) || !strings.Contains(reqs[0], `"mode":"soft"`) {
		t.Fatalf("first request %v", reqs)
	}
}

func TestRebootSoftDroppedConnection(t *testing.T) {
	fastReboot(t)
	addr, _ := startRebootPod(t, "", 1)
	var out bytes.Buffer
	if err := runReboot(&globalFlags{connection: addr}, "soft", true, &out); err != nil {
		t.Fatalf("a dropped connection after sending is success: %v", err)
	}
	if !strings.Contains(out.String(), "rebooted (soft), firmware 4.0.7") {
		t.Fatalf("output %q", out.String())
	}
}

func TestRebootNoWait(t *testing.T) {
	fastReboot(t)
	addr, pod := startRebootPod(t, `{"status":"ok","data":{"reboot":"soft"}}`, 0)
	var out bytes.Buffer
	if err := runReboot(&globalFlags{connection: addr}, "soft", false, &out); err != nil {
		t.Fatal(err)
	}
	if want := "Reboot (soft) sent to the pod at " + addr + ".\n"; out.String() != want {
		t.Fatalf("output %q, want %q", out.String(), want)
	}
	for _, r := range pod.requests() {
		if strings.Contains(r, `"cmd":"status"`) {
			t.Fatalf("--no-wait asked the pod for status: %v", pod.requests())
		}
	}
}

func TestRebootWaitGivesUp(t *testing.T) {
	fastReboot(t)
	rebootWait = 200 * time.Millisecond
	addr, _ := startRebootPod(t, "", 1000)
	err := runReboot(&globalFlags{connection: addr}, "soft", true, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "did not answer again") {
		t.Fatalf("got %v", err)
	}
}

func TestRebootHardRefused(t *testing.T) {
	fastReboot(t)
	addr, pod := startRebootPod(t, `{"status":"error","code":"BP0093"}`, 0)
	err := runReboot(&globalFlags{connection: addr}, "hard", true, &bytes.Buffer{})
	if err == nil {
		t.Fatal("hard reboot of a pod that cannot power-cycle must fail")
	}
	err = withRefusalHint(err)
	if exitCode(err) != exitRefused {
		t.Fatalf("exit code %d for %v", exitCode(err), err)
	}
	if !strings.Contains(err.Error(), "BP0093") && !strings.Contains(err.Error(), "power-cycle") {
		t.Fatalf("error %q does not name the refusal", err)
	}
	if reqs := pod.requests(); len(reqs) == 0 || !strings.Contains(reqs[0], `"mode":"hard"`) {
		t.Fatalf("requests %v", reqs)
	}
}

func TestRebootOnOlderFirmware(t *testing.T) {
	fastReboot(t)
	for _, reply := range unknownCmdReplies {
		addr, _ := startRebootPod(t, reply, 0)
		err := runReboot(&globalFlags{connection: addr}, "soft", true, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "no reboot command") || exitCode(err) != exitUpdatePod {
			t.Fatalf("got %v (exit %d)", err, exitCode(err))
		}
	}
}

func TestRebootUnreachableIsNotSent(t *testing.T) {
	fastReboot(t)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	err := runReboot(&globalFlags{connection: addr}, "soft", true, &bytes.Buffer{})
	if err == nil || exitCode(err) != exitUnreachable {
		t.Fatalf("got %v (exit %d)", err, exitCode(err))
	}
}

func TestRebootSoftAndHardConflict(t *testing.T) {
	cmd := newRebootCmd(&globalFlags{connection: "127.0.0.1:1"})
	cmd.SetArgs([]string{"--soft", "--hard"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "not both") || exitCode(err) != exitUsage {
		t.Fatalf("got %v", err)
	}
}

func TestRebootFlagsSelectMode(t *testing.T) {
	fastReboot(t)
	for _, tc := range []struct {
		args []string
		mode string
	}{
		{[]string{"--no-wait"}, "soft"},
		{[]string{"--soft", "--no-wait"}, "soft"},
		{[]string{"--hard", "--no-wait"}, "hard"},
	} {
		addr, pod := startRebootPod(t, `{"status":"ok","data":{"reboot":"x"}}`, 0)
		cmd := newRebootCmd(&globalFlags{connection: addr})
		cmd.SetArgs(tc.args)
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if reqs := pod.requests(); !strings.Contains(reqs[0], `"mode":"`+tc.mode+`"`) {
			t.Fatalf("%v: request %v", tc.args, reqs)
		}
	}
}
