package jobs

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/db"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/db/dbgen"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/media"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/storage"
)

// Webhook request headers.
const (
	HeaderTaskID        = "X-Ytdl-Task-Id"
	HeaderEvent         = "X-Ytdl-Event"
	HeaderContentSHA256 = "X-Ytdl-Content-Sha256"
	HeaderSignature     = "X-Ytdl-Signature"

	EventSucceeded = "task.succeeded"
	EventFailed    = "task.failed"
)

// Sign computes the webhook signature header value:
//
//	t=<unix seconds>,v1=<hex HMAC-SHA256(secret, "<t>.<task id>.<content sha256 hex>")>
//
// Signing the body's hash rather than the body itself lets large files be
// streamed: the receiver hashes the body as it arrives and checks both.
func Sign(secret string, t time.Time, taskID, contentSHA256 string) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac(secret, ts, taskID, contentSHA256))
}

func mac(secret, ts, taskID, contentSHA256 string) []byte {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts + "." + taskID + "." + contentSHA256))
	return m.Sum(nil)
}

var ErrBadSignature = errors.New("webhook: invalid signature")

// VerifySignature is the receiver-side check: header is the X-Ytdl-Signature
// value, contentSHA256 the hex SHA-256 the receiver computed over the body
// it actually got. Signatures older than tolerance are rejected (replays).
func VerifySignature(secret, header, taskID, contentSHA256 string, tolerance time.Duration, now time.Time) error {
	var ts, sig string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			ts = v
		case "v1":
			sig = v
		}
	}
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || sig == "" {
		return ErrBadSignature
	}
	if d := now.Sub(time.Unix(unix, 0)); d > tolerance || d < -tolerance {
		return fmt.Errorf("%w: timestamp outside tolerance", ErrBadSignature)
	}
	got, err := hex.DecodeString(sig)
	if err != nil || !hmac.Equal(got, mac(secret, ts, taskID, contentSHA256)) {
		return ErrBadSignature
	}
	return nil
}

// ErrForbiddenTarget is returned for webhook URLs the SSRF guard rejects.
var ErrForbiddenTarget = errors.New("webhook target not allowed")

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// forbiddenAddr reports addresses a webhook must never reach: the host
// itself, private networks and cloud metadata endpoints (169.254.169.254
// is link-local).
func forbiddenAddr(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() || cgnat.Contains(a)
}

// ValidateWebhookURL checks a webhook URL when a task is created. The dialer
// re-checks every resolved address at connect time, which is what actually
// stops DNS tricks; this only rejects obviously bad input early.
func ValidateWebhookURL(raw string, allowPrivate bool) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%w: must be an absolute http(s) URL", ErrForbiddenTarget)
	}
	if u.User != nil {
		return fmt.Errorf("%w: credentials in URL are not allowed", ErrForbiddenTarget)
	}
	if len(raw) > 2048 {
		return fmt.Errorf("%w: URL too long", ErrForbiddenTarget)
	}
	if allowPrivate {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".internal") {
		return fmt.Errorf("%w: %s", ErrForbiddenTarget, host)
	}
	if a, err := netip.ParseAddr(host); err == nil && forbiddenAddr(a) {
		return fmt.Errorf("%w: %s", ErrForbiddenTarget, host)
	}
	return nil
}

// NewWebhookClient returns an HTTP client for deliveries. Unless
// allowPrivate, its dialer refuses forbidden addresses after DNS resolution
// (so a public name pointing at 127.0.0.1 is still blocked). Redirects are
// not followed and environment proxies are ignored, since either could
// route around the check.
func NewWebhookClient(timeout time.Duration, allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if !allowPrivate {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			a, err := netip.ParseAddr(host)
			if err != nil || forbiddenAddr(a) {
				return fmt.Errorf("%w: %s", ErrForbiddenTarget, host)
			}
			return nil
		}
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: timeout,
			MaxIdleConnsPerHost:   2,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// webhookBackoff is the delay before retry n (1-based); the last value
// repeats.
var webhookBackoff = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute, time.Hour}

func backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > len(webhookBackoff) {
		return webhookBackoff[len(webhookBackoff)-1]
	}
	return webhookBackoff[attempt-1]
}

type failedEvent struct {
	TaskID string `json:"task_id"`
	Event  string `json:"event"`
	Status string `json:"status"`
	Error  string `json:"error"`
}

// deliver sends one webhook for task and reports whether the receiver
// accepted it (2xx).
func (r *Runner) deliver(ctx context.Context, task dbgen.Task) error {
	user, err := r.db.GetUserByID(ctx, task.UserID)
	if err != nil {
		return fmt.Errorf("load task owner: %w", err)
	}

	event, contentType := EventSucceeded, media.Format(task.Format).MIME()
	var body *os.File
	if task.Status == dbgen.TaskStatusSucceeded && task.StorageKey != nil {
		body, err = r.spool(ctx, *task.StorageKey)
		if errors.Is(err, storage.ErrNotFound) {
			body, err = nil, nil
			task.Error = ptr("file is no longer available")
			event = EventFailed
		} else if err != nil {
			return fmt.Errorf("read stored file: %w", err)
		}
	} else {
		event = EventFailed
	}
	if event == EventFailed {
		msg := "task failed"
		if task.Error != nil {
			msg = *task.Error
		}
		raw, _ := json.Marshal(failedEvent{TaskID: task.ID.String(), Event: EventFailed, Status: "failed", Error: msg})
		body, err = spoolBytes(r.workDir, raw)
		if err != nil {
			return err
		}
		contentType = "application/json"
	}
	defer func() {
		body.Close()
		os.Remove(body.Name())
	}()

	sum, size, err := hashFile(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, *task.WebhookUrl, body)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", "ytdl-service-webhook/1")
	req.Header.Set(HeaderTaskID, task.ID.String())
	req.Header.Set(HeaderEvent, event)
	req.Header.Set(HeaderContentSHA256, sum)
	req.Header.Set(HeaderSignature, Sign(user.WebhookSecret, time.Now(), task.ID.String(), sum))
	if event == EventSucceeded && task.Title != "" {
		req.Header.Set("X-Ytdl-Title", url.QueryEscape(task.Title))
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("receiver answered %s", resp.Status)
	}
	return nil
}

// spool copies a stored object to a temp file so its hash can be sent
// before the body, and so a retry does not depend on a half-read stream.
func (r *Runner) spool(ctx context.Context, key string) (*os.File, error) {
	rc, _, err := r.store.Open(ctx, key, nil)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	f, err := os.CreateTemp(r.workDir, "webhook-*")
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(f, rc); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, err
	}
	return f, nil
}

func spoolBytes(dir string, b []byte) (*os.File, error) {
	f, err := os.CreateTemp(dir, "webhook-*")
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, err
	}
	return f, nil
}

// hashFile returns the hex SHA-256 and size of f and rewinds it.
func hashFile(f *os.File) (string, int64, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// runWebhooks delivers pending webhooks until ctx is cancelled.
func (r *Runner) runWebhooks(ctx context.Context) {
	// The claim pushes next_webhook_at forward by this much, so a slow
	// delivery is not picked up a second time by another poller.
	lease := (r.cfg.WebhookTimeout + time.Minute).Seconds()
	for {
		task, err := r.db.ClaimWebhook(ctx, lease)
		switch {
		case db.IsNotFound(err):
			if !sleep(ctx, 2*time.Second) {
				return
			}
			continue
		case err != nil:
			if ctx.Err() != nil {
				return
			}
			r.log.Error("claiming webhook failed", slog.Any("err", err))
			if !sleep(ctx, 5*time.Second) {
				return
			}
			continue
		}

		l := r.log.With(slog.String("task_id", task.ID.String()), slog.Int("attempt", int(task.WebhookAttempts)))
		start := time.Now()
		derr := r.deliver(ctx, task)
		bg := context.WithoutCancel(ctx)
		switch {
		case derr == nil:
			l.Info("webhook delivered", slog.Duration("elapsed", time.Since(start)))
			if err := r.db.MarkWebhookDelivered(bg, task.ID); err != nil {
				l.Error("marking webhook delivered failed", slog.Any("err", err))
			}
		case int(task.WebhookAttempts) >= r.cfg.WebhookMaxAttempts:
			l.Warn("webhook failed permanently", slog.Any("err", derr))
			if err := r.db.MarkWebhookFailed(bg, dbgen.MarkWebhookFailedParams{ID: task.ID, WebhookError: ptr(derr.Error())}); err != nil {
				l.Error("marking webhook failed failed", slog.Any("err", err))
			}
		default:
			delay := backoff(int(task.WebhookAttempts))
			l.Warn("webhook delivery failed, will retry", slog.Duration("retry_in", delay), slog.Any("err", derr))
			if err := r.db.MarkWebhookRetry(bg, dbgen.MarkWebhookRetryParams{
				ID: task.ID, WebhookError: ptr(derr.Error()), DelaySeconds: delay.Seconds(),
			}); err != nil {
				l.Error("scheduling webhook retry failed", slog.Any("err", err))
			}
		}
	}
}

func ptr[T any](v T) *T { return &v }

// sleep waits d or until ctx is done; false means ctx is done.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
