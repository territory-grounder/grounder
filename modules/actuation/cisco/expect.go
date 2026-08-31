package cisco

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	cryptossh "golang.org/x/crypto/ssh"

	"github.com/territory-grounder/grounder/core/config"
)

// expecter reads a PTY shell's stdout and returns the accumulated bytes each time the device prompt appears
// at the tail. It runs the blocking Read in a goroutine and selects on it against ctx + a per-round timeout,
// because an SSH channel Read cannot take a deadline; the ctx watchdog (which closes the transport) is what
// actually unblocks a truly stalled Read, and this timer bounds a device that keeps dribbling output without
// ever presenting a prompt.
type expecter struct {
	r       io.Reader
	prompt  *regexp.Regexp
	timeout time.Duration
	buf     strings.Builder
	chunks  chan []byte
	errc    chan error
	started bool
}

func newExpecter(r io.Reader, prompt *regexp.Regexp, timeout time.Duration) *expecter {
	return &expecter{r: r, prompt: prompt, timeout: timeout, chunks: make(chan []byte, 1), errc: make(chan error, 1)}
}

// pump is the single reader goroutine. One goroutine for the connection's lifetime (not per until call), so
// bytes are never lost between calls; it feeds chunks to until through the channel.
func (e *expecter) pump() {
	buf := make([]byte, 4096)
	for {
		n, err := e.r.Read(buf)
		if n > 0 {
			b := make([]byte, n)
			copy(b, buf[:n])
			e.chunks <- b
		}
		if err != nil {
			e.errc <- err
			return
		}
	}
}

// until reads until the prompt regex matches the tail of everything seen SINCE THE LAST until returned, or
// the timeout/ctx fires. It returns the text accumulated in this round (including the matched prompt line),
// so the caller can strip the echoed command and the prompt.
func (e *expecter) until(ctx context.Context) (string, error) {
	if !e.started {
		e.started = true
		go e.pump()
	}
	e.buf.Reset()
	timer := time.NewTimer(e.timeout)
	defer timer.Stop()
	for {
		// A prompt already visible in what we've accumulated ends the round.
		if e.prompt.MatchString(e.buf.String()) {
			return e.buf.String(), nil
		}
		select {
		case <-ctx.Done():
			return e.buf.String(), fmt.Errorf("context cancelled while awaiting prompt: %w", ctx.Err())
		case <-timer.C:
			return e.buf.String(), fmt.Errorf("timed out after %s awaiting the device prompt", e.timeout)
		case b := <-e.chunks:
			e.buf.Write(b)
		case err := <-e.errc:
			// EOF/closed: check one last time in case the prompt arrived in the final chunk.
			if e.prompt.MatchString(e.buf.String()) {
				return e.buf.String(), nil
			}
			return e.buf.String(), fmt.Errorf("transport closed before a prompt appeared: %w", err)
		}
	}
}

// sendLine writes one CLI line + CRLF to the device stdin. Cisco CLIs expect CR; \r\n is safe on both IOS
// and ASA.
func sendLine(w io.Writer, line string) error {
	_, err := io.WriteString(w, line+"\r\n")
	return err
}

// cleanOutput strips the device's echo of the sent command (the first line, which Cisco echoes even with
// PTY ECHO off on some platforms) and the trailing prompt line, returning just the command's output bytes.
// It is lenient: if the echo isn't present it removes nothing but the prompt, so a device that suppresses
// the echo still yields clean output. The prompt regex is the RUNNER'S configured one (not the package
// default), so a wiring slice that pins a device-specific prompt strips it correctly rather than leaking it.
func cleanOutput(captured, commandLine string, prompt *regexp.Regexp) []byte {
	if prompt == nil {
		prompt = defaultPromptRE
	}
	s := strings.ReplaceAll(captured, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	// Drop a leading line that is the echoed command (allowing surrounding whitespace).
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == strings.TrimSpace(commandLine) {
		lines = lines[1:]
	}
	// Drop the trailing prompt line(s): remove trailing empty lines, then a final line that is a bare prompt.
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > 0 && prompt.MatchString(strings.TrimSpace(lines[len(lines)-1])) {
		lines = lines[:len(lines)-1]
	}
	out := strings.Join(lines, "\n")
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return nil
	}
	return []byte(out + "\n")
}

// resolveSigner resolves the device credential REFERENCE at read time and parses it in memory (INV-13): key
// material never touches a filesystem path. Every failure names the REF only, never a resolved byte.
func resolveSigner(ref config.SecretRef) (cryptossh.Signer, error) {
	material, err := ref.Resolve()
	if err != nil {
		return nil, fmt.Errorf("cisco: credential ref %q did not resolve (fail closed)", string(ref))
	}
	if strings.TrimSpace(material) == "" {
		return nil, fmt.Errorf("cisco: credential ref %q resolved empty (fail closed)", string(ref))
	}
	signer, err := cryptossh.ParsePrivateKey([]byte(material))
	if err != nil {
		return nil, fmt.Errorf("cisco: credential ref %q did not parse as a private key (fail closed)", string(ref))
	}
	return signer, nil
}
