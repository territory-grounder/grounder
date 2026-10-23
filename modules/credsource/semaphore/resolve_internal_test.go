// White-box test for the off-host URL guard. Semaphore's API returns plain arrays with no server-supplied
// follow links, so no off-host URL can arise from a normal pull — the guard is defense-in-depth (parity with
// the AWX connector's pagination-URL fix). This test exercises resolveURL directly to prove that if an
// absolute URL ever reached the request path, one pointing off the configured host+scheme is REFUSED (the
// Bearer token must never travel off-host), while a same-host absolute URL and a relative path are accepted.
package semaphore

import (
	"strings"
	"testing"
)

func TestResolveURLOffHostRefused(t *testing.T) {
	c, err := New(Config{BaseURL: "http://semaphore.internal:3000"})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	// off-host absolute URL → refused, unmistakably.
	if _, err := c.resolveURL("http://evil.example.com/api/projects"); err == nil || !strings.Contains(err.Error(), "off-host") {
		t.Fatalf("off-host url must be refused with an off-host error, got: %v", err)
	}
	// scheme mismatch (https vs configured http) on the same host → refused.
	if _, err := c.resolveURL("https://semaphore.internal:3000/api/projects"); err == nil || !strings.Contains(err.Error(), "off-host") {
		t.Fatalf("scheme mismatch must be refused, got: %v", err)
	}
	// same host+scheme absolute → accepted.
	if got, err := c.resolveURL("http://semaphore.internal:3000/api/ping"); err != nil || got != "http://semaphore.internal:3000/api/ping" {
		t.Fatalf("same-host absolute must be accepted, got %q err=%v", got, err)
	}
	// relative path → joined onto the base.
	if got, err := c.resolveURL("/api/projects"); err != nil || got != "http://semaphore.internal:3000/api/projects" {
		t.Fatalf("relative path join wrong, got %q err=%v", got, err)
	}
}
