package jobs

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSignKnownVector(t *testing.T) {
	// Fixed vector (computed independently with Python hmac) so receivers in
	// other languages can check their code:
	// HMAC-SHA256("secret", "1700000000.task-1.abc").
	got := Sign("secret", time.Unix(1700000000, 0), "task-1", "abc")
	want := "t=1700000000,v1=9ca22190c6498378a48a9032f7e5f18d414d2485a9fca75d77b173b799c48fb2"
	if got != want {
		t.Fatalf("Sign = %s\nwant   %s", got, want)
	}
}

func TestVerifySignature(t *testing.T) {
	now := time.Unix(1700000000, 0)
	h := Sign("secret", now, "task-1", "abc")

	if err := VerifySignature("secret", h, "task-1", "abc", 5*time.Minute, now.Add(time.Minute)); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	for name, tc := range map[string]struct {
		secret, header, task, sum string
		at                        time.Time
	}{
		"wrong secret":  {"other", h, "task-1", "abc", now},
		"wrong task":    {"secret", h, "task-2", "abc", now},
		"tampered body": {"secret", h, "task-1", "abd", now},
		"replayed late": {"secret", h, "task-1", "abc", now.Add(10 * time.Minute)},
		"garbage":       {"secret", "nonsense", "task-1", "abc", now},
		"no v1":         {"secret", "t=1700000000", "task-1", "abc", now},
	} {
		if err := VerifySignature(tc.secret, tc.header, tc.task, tc.sum, 5*time.Minute, tc.at); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: got %v, want ErrBadSignature", name, err)
		}
	}
}

func TestValidateWebhookURL(t *testing.T) {
	for _, u := range []string{"https://example.com/hook", "http://hooks.example.org:8443/x?y=1"} {
		if err := ValidateWebhookURL(u, false); err != nil {
			t.Errorf("%s rejected: %v", u, err)
		}
	}
	for _, u := range []string{
		"ftp://example.com", "/relative", "https://user:pw@example.com",
		"http://127.0.0.1/x", "http://localhost:8080", "http://10.1.2.3", "http://169.254.169.254/latest/meta-data",
		"http://[::1]/", "http://192.168.0.10", "http://metadata.google.internal/", "http://100.64.0.1",
	} {
		if err := ValidateWebhookURL(u, false); !errors.Is(err, ErrForbiddenTarget) {
			t.Errorf("%s: got %v, want ErrForbiddenTarget", u, err)
		}
	}
	if err := ValidateWebhookURL("http://127.0.0.1/x", true); err != nil {
		t.Errorf("allowPrivate still rejected loopback: %v", err)
	}
}

// The dialer guard is what stops DNS names that resolve to private
// addresses; a loopback test server stands in for such a target.
func TestWebhookClientBlocksPrivateAtDial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()

	ctx := context.Background()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, nil)
	if _, err := NewWebhookClient(5*time.Second, false).Do(req); !errors.Is(err, ErrForbiddenTarget) {
		t.Fatalf("guarded client reached loopback: %v", err)
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, nil)
	resp, err := NewWebhookClient(5*time.Second, true).Do(req)
	if err != nil {
		t.Fatalf("unguarded client: %v", err)
	}
	resp.Body.Close()
}

func TestWebhookClientDoesNotFollowRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/", http.StatusFound)
	}))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL, nil)
	resp, err := NewWebhookClient(5*time.Second, true).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status %d: redirect was followed", resp.StatusCode)
	}
}

func TestBackoff(t *testing.T) {
	if backoff(1) != 30*time.Second || backoff(3) != 10*time.Minute || backoff(99) != time.Hour || backoff(0) != 30*time.Second {
		t.Fatal("unexpected backoff schedule")
	}
}
