package ytdl

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	ytdlp "github.com/lrstanley/go-ytdlp"
)

// go-ytdlp downloads the yt-dlp binary with a hard 30 s HTTP timeout for the
// whole file (~35 MB), which fails outright on links slower than ~1.2 MB/s.
// EnsureBinary keeps go-ytdlp's install model (binary in its cache dir,
// self-updated later) but does the download itself, without that limit.

const (
	releaseBase      = "https://github.com/yt-dlp/yt-dlp/releases/download/"
	downloadAttempts = 3
	attemptTimeout   = 10 * time.Minute
)

// EnsureBinary makes a yt-dlp binary available to go-ytdlp and returns its
// path. An existing binary (go-ytdlp cache, then PATH) is used as is.
// Otherwise the release go-ytdlp pins is downloaded into the go-ytdlp cache
// under the name go-ytdlp looks for, verified against the release's
// SHA2-256SUMS, with retries.
func EnsureBinary(ctx context.Context, log *slog.Logger) (string, error) {
	noDownload := &ytdlp.InstallOptions{DisableDownload: true, AllowVersionMismatch: true}
	if res, err := ytdlp.Install(ctx, noDownload); err == nil {
		return res.Executable, nil
	}

	asset, ok := releaseAsset()
	if !ok {
		// Not a platform we map ourselves: let go-ytdlp try.
		res, err := ytdlp.Install(ctx, &ytdlp.InstallOptions{AllowVersionMismatch: true})
		if err != nil {
			return "", err
		}
		return res.Executable, nil
	}
	dir, err := ytdlp.GetCacheDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create yt-dlp cache dir: %w", err)
	}

	d := downloader{
		client:  &http.Client{}, // no overall timeout: per-attempt context instead
		baseURL: releaseBase + ytdlp.Version + "/",
		log:     log,
	}
	dest := filepath.Join(dir, "yt-dlp-"+ytdlp.Version)
	if err := d.fetchVerified(ctx, asset, dest); err != nil {
		return "", err
	}

	res, err := ytdlp.Install(ctx, noDownload)
	if err != nil {
		return "", fmt.Errorf("yt-dlp downloaded to %s but not found by go-ytdlp: %w", dest, err)
	}
	return res.Executable, nil
}

// releaseAsset mirrors go-ytdlp's choice of release file for Linux.
func releaseAsset() (string, bool) {
	if runtime.GOOS != "linux" {
		return "", false
	}
	musl := isMusl()
	switch {
	case runtime.GOARCH == "amd64" && musl:
		return "yt-dlp_musllinux", true
	case runtime.GOARCH == "arm64" && musl:
		return "yt-dlp_musllinux_aarch64", true
	case runtime.GOARCH == "amd64":
		return "yt-dlp_linux", true
	case runtime.GOARCH == "arm64":
		return "yt-dlp_linux_aarch64", true
	}
	return "", false
}

// isMusl uses the same probe as go-ytdlp, so both agree on the asset.
func isMusl() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ldd", "/bin/ls").CombinedOutput()
	return err == nil && strings.Contains(string(out), "musl")
}

type downloader struct {
	client  *http.Client
	baseURL string
	log     *slog.Logger
}

// fetchVerified downloads baseURL+asset to dest, checking it against the
// release checksums. dest only ever appears complete and verified.
func (d downloader) fetchVerified(ctx context.Context, asset, dest string) error {
	var lastErr error
	for attempt := 1; attempt <= downloadAttempts; attempt++ {
		start := time.Now()
		actx, cancel := context.WithTimeout(ctx, attemptTimeout)
		lastErr = d.fetchOnce(actx, asset, dest)
		cancel()
		if lastErr == nil {
			d.log.Info("yt-dlp downloaded", slog.String("asset", asset), slog.Int("attempt", attempt),
				slog.Duration("elapsed", time.Since(start)))
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		d.log.Warn("yt-dlp download failed", slog.String("asset", asset),
			slog.Int("attempt", attempt), slog.Int("of", downloadAttempts), slog.Any("err", lastErr))
		if attempt < downloadAttempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 5 * time.Second):
			}
		}
	}
	return fmt.Errorf("download yt-dlp (%d attempts): %w", downloadAttempts, lastErr)
}

func (d downloader) fetchOnce(ctx context.Context, asset, dest string) error {
	want, err := d.expectedSum(ctx, asset)
	if err != nil {
		return err
	}

	body, err := d.get(ctx, asset)
	if err != nil {
		return err
	}
	defer body.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".yt-dlp-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after the rename

	h := sha256.New()
	pr := &progressReader{r: body, log: d.log, asset: asset, last: time.Now()}
	if _, err := io.Copy(io.MultiWriter(tmp, h), pr); err != nil {
		tmp.Close()
		return fmt.Errorf("download %s: %w", asset, err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("%s checksum mismatch: got %s, want %s", asset, got, want)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dest)
}

// expectedSum reads the asset's SHA-256 from the release's SHA2-256SUMS.
func (d downloader) expectedSum(ctx context.Context, asset string) (string, error) {
	body, err := d.get(ctx, "SHA2-256SUMS")
	if err != nil {
		return "", err
	}
	defer body.Close()
	sc := bufio.NewScanner(io.LimitReader(body, 1<<20))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == asset && len(f[0]) == 64 {
			return strings.ToLower(f[0]), nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("read SHA2-256SUMS: %w", err)
	}
	return "", fmt.Errorf("SHA2-256SUMS has no entry for %s", asset)
}

func (d downloader) get(ctx context.Context, name string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.baseURL+name, http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", name, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("get %s: %s", name, resp.Status)
	}
	return resp.Body, nil
}

// progressReader logs download progress every 10 s, so a slow first start
// is visibly progressing rather than hung.
type progressReader struct {
	r     io.Reader
	log   *slog.Logger
	asset string
	n     int64
	last  time.Time
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.n += int64(n)
	if time.Since(p.last) >= 10*time.Second {
		p.last = time.Now()
		p.log.Info("downloading yt-dlp", slog.String("asset", p.asset), slog.Int64("bytes", p.n))
	}
	return n, err
}
