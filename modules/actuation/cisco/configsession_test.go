package cisco

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	cryptossh "golang.org/x/crypto/ssh"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/safety"
)

// serveFakeCiscoConfig serves ONE SSH connection acting as an IOS/ASA device that supports CONFIG mode. It
// records the config lines it received (in order), enters/leaves config on `configure terminal`/`end`, and —
// when badLine is non-empty and a config line equals it — answers with a Cisco rejection ("% Invalid input")
// exactly as a real device does. Shares the read harness's SSH helpers (genSigner/writeKnownHosts/loopbackPipe).
// fakeDeviceState is device state that must persist ACROSS sessions — the commit-confirmed flow applies under a
// revert timer in one session and commits with `configure confirm` in a SEPARATE session, so the armed-revert
// bit cannot live in a single connection's local scope.
type fakeDeviceState struct {
	mu     sync.Mutex
	revert bool
}

func (s *fakeDeviceState) setRevert(v bool) { s.mu.Lock(); s.revert = v; s.mu.Unlock() }
func (s *fakeDeviceState) revertPending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revert
}

func serveFakeCiscoConfig(t *testing.T, conn net.Conn, hostSigner cryptossh.Signer, wantClientPub cryptossh.PublicKey, badLine string, refuseConfig bool, st *fakeDeviceState, gotLines chan<- string) {
	t.Helper()
	cfg := &cryptossh.ServerConfig{
		PublicKeyCallback: func(_ cryptossh.ConnMetadata, key cryptossh.PublicKey) (*cryptossh.Permissions, error) {
			if !bytes.Equal(key.Marshal(), wantClientPub.Marshal()) {
				return nil, errors.New("unknown client key")
			}
			return nil, nil
		},
	}
	cfg.AddHostKey(hostSigner)
	sc, chans, reqs, err := cryptossh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer func() { _ = sc.Close() }()
	go cryptossh.DiscardRequests(reqs)
	const prompt = "ciscoasa# "
	const cfgPrompt = "ciscoasa(config)# "
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(cryptossh.UnknownChannelType, "only session")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			return
		}
		go func() {
			for req := range chReqs {
				switch req.Type {
				case "pty-req", "env":
					_ = req.Reply(true, nil)
				case "shell":
					_ = req.Reply(true, nil)
					_, _ = ch.Write([]byte(prompt))
				default:
					_ = req.Reply(false, nil)
				}
			}
		}()
		inConfig := false
		var line bytes.Buffer
		b := make([]byte, 256)
		for {
			n, rerr := ch.Read(b)
			if n > 0 {
				line.Write(b[:n])
				for {
					s := line.String()
					idx := strings.IndexAny(s, "\r\n")
					if idx < 0 {
						break
					}
					cmd := strings.TrimSpace(s[:idx])
					line.Reset()
					line.WriteString(strings.TrimLeft(s[idx:], "\r\n"))
					if cmd == "" {
						continue
					}
					_, _ = ch.Write([]byte(cmd + "\r\n")) // echo, as a real device does
					switch {
					case cmd == "exit":
						_ = ch.Close()
						return
					case strings.HasPrefix(cmd, "terminal "):
						_, _ = ch.Write([]byte(prompt)) // pager-off is sent pre-config
					case cmd == "configure terminal":
						if refuseConfig {
							// Simulate a device that SILENTLY stays in exec mode (no `%` error, no config prompt).
							_, _ = ch.Write([]byte(prompt))
						} else {
							inConfig = true
							_, _ = ch.Write([]byte(cfgPrompt))
						}
					case strings.HasPrefix(cmd, "configure terminal revert timer "):
						// IOS commit-confirmed entry: enters config AND arms an auto-revert (persisted state).
						inConfig = true
						st.setRevert(true)
						_, _ = ch.Write([]byte(cfgPrompt))
					case cmd == "configure confirm":
						// EXEC command (not config mode): commits the change, cancelling the pending revert.
						if st.revertPending() {
							st.setRevert(false)
							_, _ = ch.Write([]byte(prompt))
						} else {
							_, _ = ch.Write([]byte("% No rollback configuration is pending\r\n" + prompt))
						}
					case cmd == "end":
						inConfig = false
						_, _ = ch.Write([]byte(prompt))
					case inConfig:
						gotLines <- cmd
						if badLine != "" && cmd == badLine {
							_, _ = ch.Write([]byte("% Invalid input detected at '^' marker.\r\n" + cfgPrompt))
						} else {
							_, _ = ch.Write([]byte(cfgPrompt))
						}
					default:
						_, _ = ch.Write([]byte(prompt))
					}
				}
			}
			if rerr != nil {
				return
			}
		}
	}
}

// ciscoTestConfigRunner builds a configRunner whose dial reaches an in-process serveFakeCiscoConfig, plus a
// channel of the config lines the fake device received.
func ciscoTestConfigRunner(t *testing.T, badLine string, refuseConfig bool) (*configRunner, <-chan string) {
	t.Helper()
	hostSigner, _ := genSigner(t)
	clientSigner, clientPEM := genSigner(t)
	t.Setenv("CISCO_TEST_KEY", string(clientPEM))
	kh := writeKnownHosts(t, "cisco-dev:22", hostSigner.PublicKey())

	gotLines := make(chan string, 8)
	st := &fakeDeviceState{} // one device state, SHARED across every session this runner dials (revert then confirm)
	dev := Device{
		Host: "cisco-dev", Identity: "netops", KeyRef: config.SecretRef("env:CISCO_TEST_KEY"),
		KnownHosts: kh,
	}
	r := NewInteractiveRunner(dev)
	r.connectTimeout = 3 * time.Second
	r.ioTimeout = 3 * time.Second
	r.dial = func(_ context.Context, _ string) (net.Conn, error) {
		client, server := loopbackPipe(t)
		go serveFakeCiscoConfig(t, server, hostSigner, clientSigner.PublicKey(), badLine, refuseConfig, st, gotLines)
		return client, nil
	}
	return &configRunner{r: r}, gotLines
}

// drainLines reads whatever the fake device has recorded so far (non-blocking). Safe to call once RunConfig has
// returned: every line the device saw was recorded before the runner read the prompt that let it proceed.
func drainLines(ch <-chan string) []string {
	var out []string
	for {
		select {
		case s := <-ch:
			out = append(out, s)
		default:
			return out
		}
	}
}

// The end-to-end write transport: dial → pty+shell → pager-off → `configure terminal` → apply each line → `end`.
func TestRunConfigAppliesLinesInConfigMode(t *testing.T) {
	cr, gotLines := ciscoTestConfigRunner(t, "", false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := cr.RunConfig(ctx, []string{"interface Gi0/1", "description tg-managed"})
	if err != nil {
		t.Fatalf("RunConfig: %v", err)
	}
	got := drainLines(gotLines)
	if len(got) != 2 || got[0] != "interface Gi0/1" || got[1] != "description tg-managed" {
		t.Fatalf("device applied the wrong lines: %v", got)
	}
	if !strings.Contains(string(res.Stdout), "(config)") {
		t.Errorf("transcript should show the device entered config mode; got %q", res.Stdout)
	}
}

// KILLING the fail-closed sensor: the device rejects a line with "% Invalid input"; RunConfig must stop, error,
// and NOT send the line after the rejected one.
func TestRunConfigFailsClosedOnDeviceReject(t *testing.T) {
	cr, gotLines := ciscoTestConfigRunner(t, "bogus-command", false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := cr.RunConfig(ctx, []string{"interface Gi0/1", "bogus-command", "description never-applied"})
	if err == nil {
		t.Fatal("must fail closed when the device rejects a line")
	}
	got := drainLines(gotLines)
	if len(got) != 2 {
		t.Fatalf("device must have seen exactly the good + rejected line (not the one after); got %v", got)
	}
	for _, l := range got {
		if l == "description never-applied" {
			t.Fatalf("a line AFTER the rejected one reached the device: %v", got)
		}
	}
}

func TestRunConfigRefusesEmpty(t *testing.T) {
	cr, _ := ciscoTestConfigRunner(t, "", false)
	if _, err := cr.RunConfig(context.Background(), nil); err == nil {
		t.Fatal("an empty change must be refused before any dial")
	}
}

// POSITIVE config-mode gate: a device that silently stays in exec mode after `configure terminal` (no `%`
// error, no config prompt) must fail closed BEFORE any config line is sent as an exec command.
func TestRunConfigFailsClosedIfConfigModeNotEntered(t *testing.T) {
	cr, gotLines := ciscoTestConfigRunner(t, "", true) // refuseConfig: device never enters config mode
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := cr.RunConfig(ctx, []string{"interface Gi0/1"})
	if err == nil {
		t.Fatal("must fail closed if the device did not enter config mode")
	}
	if got := drainLines(gotLines); len(got) != 0 {
		t.Fatalf("no config line may be sent when config mode was not entered; device saw %v", got)
	}
}

// The full write path: WriteModule (mode gate + config-line allowlist/guard) driving the REAL config transport
// against the fake device. Proves slice 1's gate and slice 2's transport compose.
func TestWriteModuleDrivesConfigRunnerEndToEnd(t *testing.T) {
	cr, gotLines := ciscoTestConfigRunner(t, "", false)
	m := NewWriteModule(cr, safety.NewActuatingChokepoint(), []string{"interface ", "description "})
	if m.ReadOnly() {
		t.Fatal("armed WriteModule with a real runner + allowlist must not be read-only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := m.Exec(ctx, []string{"interface", "Gi0/1"}, []byte("description tg-managed\n"))
	if err != nil {
		t.Fatalf("armed write through the real transport: %v", err)
	}
	got := drainLines(gotLines)
	if len(got) != 2 || got[0] != "interface Gi0/1" || got[1] != "description tg-managed" {
		t.Fatalf("WriteModule -> configRunner applied the wrong lines: %v", got)
	}
}

func TestDeviceErrorDetectsCiscoRejections(t *testing.T) {
	for _, s := range []string{"% Invalid input detected at '^' marker.", "  % Incomplete command.", "% Ambiguous command:  \"sh\""} {
		if deviceError("interface Gi0/1\r\n"+s+"\r\nciscoasa(config)# ") == "" {
			t.Errorf("deviceError missed a Cisco rejection: %q", s)
		}
	}
	if deviceError("interface Gi0/1\r\nciscoasa(config)# ") != "" {
		t.Error("deviceError flagged a clean transcript")
	}
}
