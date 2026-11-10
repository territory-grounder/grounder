package loadharness

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/territory-grounder/grounder/core/httpapi"
)

// client is the harness's authenticated HTTP side. It speaks the two machine credentials the front door
// accepts and NOTHING privileged: ingest POSTs carry the per-source static bearer (AuthIngestPush) when
// one is configured, else an HMAC signature; the sessions read-back is always HMAC (the read-only tier's
// machine path). Response DTOs are core/httpapi's own types — never a re-typed copy that could drift.
type client struct {
	base        string
	http        *http.Client
	sourceType  string
	ingestToken string
	hmacSource  string
	hmacSecret  []byte
}

func newClient(cfg Config) *client {
	return &client{
		base:        cfg.BaseURL,
		http:        &http.Client{Timeout: 30 * time.Second},
		sourceType:  cfg.SourceType,
		ingestToken: cfg.IngestToken,
		hmacSource:  cfg.HMACSource,
		hmacSecret:  cfg.HMACSecret,
	}
}

// sign stamps the four X-TG HMAC headers exactly as core/auth verifies them: sha256-HMAC over
// timestamp\n nonce\n body, a fresh random nonce per request (the verifier's replay protection burns
// each one), the timestamp in unix seconds.
func (c *client) sign(req *http.Request, body []byte) error {
	var nb [16]byte
	if _, err := rand.Read(nb[:]); err != nil {
		return fmt.Errorf("nonce: %w", err)
	}
	nonce := hex.EncodeToString(nb[:])
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, c.hmacSecret)
	mac.Write([]byte(ts))
	mac.Write([]byte("\n"))
	mac.Write([]byte(nonce))
	mac.Write([]byte("\n"))
	mac.Write(body)
	req.Header.Set("X-TG-Source", c.hmacSource)
	req.Header.Set("X-TG-Timestamp", ts)
	req.Header.Set("X-TG-Nonce", nonce)
	req.Header.Set("X-TG-Signature", hex.EncodeToString(mac.Sum(nil)))
	return nil
}

// postIncident delivers one synthetic webhook to POST /v1/ingest/{source_type} and decodes the batch
// response (the Alertmanager module is a BatchIngester, so the front door always answers in the batch
// shape). Any non-202 is an error carrying the status and a bounded body snippet — the harness records
// it as a failed run, it never retries (a retry would measure the retry, not the pipeline).
func (c *client) postIncident(ctx context.Context, body []byte) (httpapi.IngestBatchAccepted, error) {
	var out httpapi.IngestBatchAccepted
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/ingest/"+c.sourceType, bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.ingestToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.ingestToken)
	} else if err := c.sign(req, body); err != nil {
		return out, err
	}
	if err := c.do(req, http.StatusAccepted, &out); err != nil {
		return out, err
	}
	return out, nil
}

// sessions reads the spine's recent-sessions page (GET /v1/sessions?limit=N), HMAC-signed.
func (c *client) sessions(ctx context.Context, limit int) (httpapi.SessionsPage, error) {
	var out httpapi.SessionsPage
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/v1/sessions?limit=%d", c.base, limit), nil)
	if err != nil {
		return out, err
	}
	if err := c.sign(req, nil); err != nil {
		return out, err
	}
	if err := c.do(req, http.StatusOK, &out); err != nil {
		return out, err
	}
	return out, nil
}

// sessionDetail reads ONE session's detail (GET /v1/sessions/{ref}, HMAC-signed — a machine principal
// satisfies AuthTraceRead as a trusted system caller), the surface that carries the session's STATUS.
// TG-80 P1-2: the recent-sessions page appears at CLASSIFICATION, the earliest boundary; only the detail
// says whether the session reached a terminal (proposed | executed | stopped), which is what "poll to
// terminal" means.
func (c *client) sessionDetail(ctx context.Context, ref string) (httpapi.SessionDetailDTO, error) {
	var out httpapi.SessionDetailDTO
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/v1/sessions/"+url.PathEscape(ref), nil)
	if err != nil {
		return out, err
	}
	if err := c.sign(req, nil); err != nil {
		return out, err
	}
	if err := c.do(req, http.StatusOK, &out); err != nil {
		return out, err
	}
	return out, nil
}

// do executes, enforces the expected status, and decodes JSON. The error body is bounded to one line's
// worth — enough to name the refusal, never a transcript.
func (c *client) do(req *http.Request, wantStatus int, into any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("%s %s: status %d (want %d): %s",
			req.Method, req.URL.Path, resp.StatusCode, wantStatus, bytes.TrimSpace(snippet))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(into); err != nil {
		return fmt.Errorf("%s %s: decode: %w", req.Method, req.URL.Path, err)
	}
	return nil
}
