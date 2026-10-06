package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/api"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/app"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/cache"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/db"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/db/dbtest"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/jobs"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/media"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/storage"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/storage/local"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/ytdl"
)

// These tests run the real router, Postgres, Redis, workers and webhook
// delivery against a fake media backend (no yt-dlp, no network):
//
//	docker run -d --rm --name ytdl-pg-test -e POSTGRES_PASSWORD=test -p 15432:5432 postgres:17-alpine
//	docker run -d --rm --name ytdl-redis-test -p 16379:6379 redis:7-alpine
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:15432/postgres?sslmode=disable' \
//	TEST_REDIS_URL='redis://localhost:16379/15' go test ./internal/api -v
//
// TEST_REDIS_URL's database is flushed by every test; point it at a
// throwaway instance.

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeMedia stands in for media.Service: deterministic IDs and bytes, and
// counters that show whether yt-dlp would have run.
type fakeMedia struct {
	store storage.Storage

	mu       sync.Mutex
	resolves int
	fetches  map[string]int
	queries  []string
}

func newFakeMedia(store storage.Storage) *fakeMedia {
	return &fakeMedia{store: store, fetches: map[string]int{}}
}

func fakeID(query string) string {
	if id := app.YouTubeID(query); id != "" {
		return id
	}
	sum := sha256.Sum256([]byte(query))
	return "v" + hex.EncodeToString(sum[:5])
}

func fileBytes(id string, f media.Format) []byte {
	return bytes.Repeat([]byte(id+"|"+string(f)+"|"), 400)
}

func liveBytes(id string, f media.Format) []byte {
	return []byte("LIVE:" + id + ":" + string(f))
}

func (m *fakeMedia) Resolve(ctx context.Context, query string, f media.Format) (media.Source, error) {
	m.mu.Lock()
	m.resolves++
	m.queries = append(m.queries, query)
	m.mu.Unlock()
	if strings.Contains(query, "unavailable") {
		return media.Source{}, fmt.Errorf("yt-dlp: video unavailable")
	}
	id := fakeID(query)
	src := media.Source{
		Info:   ytdl.Info{ID: id, Title: "Title " + id, WebpageURL: "https://www.youtube.com/watch?v=" + id, Duration: 42},
		Format: f,
		Key:    media.Key(id, f),
	}
	if obj, err := m.store.Stat(ctx, src.Key); err == nil {
		src.Stored = &obj
	}
	return src, nil
}

func (m *fakeMedia) Lookup(ctx context.Context, id string, f media.Format) (media.Source, bool, error) {
	obj, err := m.store.Stat(ctx, media.Key(id, f))
	if err != nil {
		return media.Source{}, false, nil
	}
	return media.Source{
		Info:   ytdl.Info{ID: id, Title: obj.Meta["title"], WebpageURL: obj.Meta["source-url"]},
		Format: f, Key: obj.Key, Stored: &obj,
	}, true, nil
}

func (m *fakeMedia) Fetch(ctx context.Context, src media.Source) (media.Fetched, error) {
	if src.Stored != nil {
		return media.Fetched{Object: *src.Stored}, nil
	}
	m.mu.Lock()
	m.fetches[src.Key]++
	m.mu.Unlock()
	obj, err := m.store.Put(ctx, src.Key, bytes.NewReader(fileBytes(src.Info.ID, src.Format)), storage.ObjectInfo{
		ContentType: src.Format.MIME(),
		Meta:        map[string]string{"title": src.Info.Title, "source-url": src.Info.WebpageURL},
	})
	if err != nil {
		return media.Fetched{}, err
	}
	return media.Fetched{Object: obj, Downloaded: true}, nil
}

func (m *fakeMedia) Stream(_ context.Context, src media.Source, w io.Writer) error {
	_, err := w.Write(liveBytes(src.Info.ID, src.Format))
	return err
}

func (m *fakeMedia) resolvedQueries() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.queries...)
}

func (m *fakeMedia) counts(key string) (resolves, fetches int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.resolves, m.fetches[key]
}

type env struct {
	t        *testing.T
	handler  http.Handler
	db       *db.DB
	store    storage.Storage
	fake     *fakeMedia
	runner   *jobs.Runner
	admin    string // API keys
	user     string
	userID   int64
	adminID  int64
	cacheTTL time.Duration
}

type envOpts struct {
	resultTTL          time.Duration
	rateLimit, burst   int
	webhookMaxAttempts int
}

func newEnv(t *testing.T, o envOpts) *env {
	t.Helper()
	redisURL := os.Getenv("TEST_REDIS_URL")
	if redisURL == "" {
		t.Skip("TEST_REDIS_URL not set")
	}
	database := dbtest.New(t)
	ropts, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(ropts)
	t.Cleanup(func() { rdb.Close() })
	if err := rdb.FlushDB(context.Background()).Err(); err != nil {
		t.Fatalf("redis: %v", err)
	}

	if o.resultTTL == 0 {
		o.resultTTL = time.Hour
	}
	if o.rateLimit == 0 {
		o.rateLimit, o.burst = 10000, 10000
	}
	if o.webhookMaxAttempts == 0 {
		o.webhookMaxAttempts = 3
	}

	store, err := local.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fake := newFakeMedia(store)
	c := cache.New(rdb, time.Minute, discard)
	a := app.New(fake, store, database, c, discard)
	runner := jobs.New(a, jobs.Config{
		Workers: 2, TaskLease: 30 * time.Second, TaskResultTTL: o.resultTTL,
		FileIdleTTL: 168 * time.Hour, CleanupInterval: time.Hour,
		WebhookTimeout: 5 * time.Second, WebhookMaxAttempts: o.webhookMaxAttempts, WebhookAllowPrivate: true,
		WorkDir: t.TempDir(),
	}, discard)
	ctx, cancel := context.WithCancel(context.Background())
	runner.Start(ctx)
	t.Cleanup(func() { cancel(); runner.Wait() })

	srv := api.New(a, rdb, runner, api.Config{
		RateLimitPerMinute: o.rateLimit, RateLimitBurst: o.burst, WebhookAllowPrivate: true,
	}, discard)

	e := &env{t: t, handler: srv.Handler(), db: database, store: store, fake: fake, runner: runner}
	e.admin, _, err = database.BootstrapAdmin(context.Background(), "admin", "admin@example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	e.adminID = intOf(e.json(e.do("GET", "/v1/me", e.admin, ""), 200)["id"])

	res := e.json(e.do("POST", "/v1/users", e.admin, `{"username":"alice","email":"alice@example.com"}`), 201)
	e.user = res["api_key"].(map[string]any)["key"].(string)
	e.userID = int64(res["user"].(map[string]any)["id"].(float64))
	return e
}

// intOf reads a JSON number as an ID.
func intOf(v any) int64 { return int64(v.(float64)) }

func (e *env) do(method, path, key, body string, hdr ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	e.handler.ServeHTTP(w, req)
	// Every response the tests see must match the documented contract.
	validateResponse(e.t, req, w)
	return w
}

func (e *env) json(w *httptest.ResponseRecorder, wantStatus int) map[string]any {
	e.t.Helper()
	if w.Code != wantStatus {
		e.t.Fatalf("status %d, want %d; body: %s", w.Code, wantStatus, w.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		e.t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return m
}

func (e *env) waitTask(key, id string, done func(map[string]any) bool) map[string]any {
	e.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		t := e.json(e.do("GET", "/v1/tasks/"+id, key, ""), 200)
		if done(t) {
			return t
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("task did not reach expected state: %v", t)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func finished(t map[string]any) bool { return t["status"] == "succeeded" || t["status"] == "failed" }

const ytURL = "https://www.youtube.com/watch?v=dQw4w9WgXcQ"

func filesPath(rawURL, name, format string) string {
	q := url.Values{"format": {format}}
	if rawURL != "" {
		q.Set("url", rawURL)
	}
	if name != "" {
		q.Set("name", name)
	}
	return "/v1/files?" + q.Encode()
}

func TestAuthAndRoles(t *testing.T) {
	e := newEnv(t, envOpts{})
	if w := e.do("GET", "/v1/me", "", ""); w.Code != 401 {
		t.Errorf("no key: %d", w.Code)
	}
	if w := e.do("GET", "/v1/me", "ytdl_bogusbogusbogusbogusbogusbogusbogusbogus", ""); w.Code != 401 {
		t.Errorf("bad key: %d", w.Code)
	}
	if w := e.do("GET", "/v1/me", "", "", "X-API-Key", e.user); w.Code != 200 {
		t.Errorf("X-API-Key header: %d", w.Code)
	}
	for _, path := range []string{"/v1/users", "/v1/videos", "/v1/audio", fmt.Sprintf("/v1/users/%d/api-keys", e.userID)} {
		if w := e.do("GET", path, e.user, ""); w.Code != 403 {
			t.Errorf("user GET %s: %d, want 403", path, w.Code)
		}
	}
	if w := e.do("POST", "/v1/users", e.user, `{"username":"bob","email":"bob@example.com"}`); w.Code != 403 {
		t.Errorf("user create: %d, want 403", w.Code)
	}
	if w := e.do("DELETE", fmt.Sprintf("/v1/users/%d", e.adminID), e.user, ""); w.Code != 403 {
		t.Errorf("user delete: %d, want 403", w.Code)
	}
	if w := e.do("GET", "/v1/users", e.admin, ""); w.Code != 200 {
		t.Errorf("admin list users: %d", w.Code)
	}
	me := e.json(e.do("GET", "/v1/me", e.user, ""), 200)
	if _, leaked := me["webhook_secret"]; leaked {
		t.Error("/me leaks webhook_secret")
	}
}

func TestFileIsNotRefetched(t *testing.T) {
	e := newEnv(t, envOpts{})
	key := media.Key("dQw4w9WgXcQ", media.MP3)

	w := e.do("GET", filesPath(ytURL, "", "mp3"), e.user, "")
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), fileBytes("dQw4w9WgXcQ", media.MP3)) {
		t.Fatalf("first GET: %d %q", w.Code, w.Body.String()[:min(40, w.Body.Len())])
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") || !strings.Contains(cd, "Title dQw4w9WgXcQ.mp3") {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if r, f := e.fake.counts(key); r != 1 || f != 1 {
		t.Fatalf("after first GET: resolves=%d fetches=%d", r, f)
	}

	w = e.do("GET", filesPath(ytURL, "", "mp3"), e.user, "")
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), fileBytes("dQw4w9WgXcQ", media.MP3)) {
		t.Fatalf("second GET: %d", w.Code)
	}
	// A YouTube URL maps straight to the stored file: no resolve, no fetch.
	if r, f := e.fake.counts(key); r != 1 || f != 1 {
		t.Fatalf("second GET re-fetched: resolves=%d fetches=%d", r, f)
	}

	// Catalog row exists and was touched.
	a := e.json(e.do("GET", "/v1/audio/dQw4w9WgXcQ/mp3", e.admin, ""), 200)
	if a["stored"] != true || a["storage_key"] != key {
		t.Fatalf("catalog row: %v", a)
	}

	// Names: the second request is answered from the resolve cache.
	e.do("GET", filesPath("", "never gonna", "mp3"), e.user, "")
	before, _ := e.fake.counts("")
	if w := e.do("GET", filesPath("", "Never Gonna ", "mp3"), e.user, ""); w.Code != 200 {
		t.Fatalf("name GET: %d", w.Code)
	}
	if after, _ := e.fake.counts(""); after != before {
		t.Fatalf("repeated name resolved again (%d -> %d)", before, after)
	}

	h := e.json(e.do("GET", "/v1/history?sort=created_at", e.user, ""), 200)
	items := h["items"].([]any)
	if len(items) != 4 {
		t.Fatalf("history has %d items", len(items))
	}
	first, second := items[0].(map[string]any), items[1].(map[string]any)
	if first["from_cache"] != false || second["from_cache"] != true || second["status"] != "ok" || intOf(second["http_status"]) != 200 {
		t.Fatalf("history rows: %v / %v", first, second)
	}
}

func TestURLTakesPriorityOverName(t *testing.T) {
	e := newEnv(t, envOpts{})
	w := e.do("GET", filesPath("https://vimeo.com/12345", "ignored name", "wav"), e.user, "")
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if q := e.fake.resolvedQueries(); len(q) != 1 || q[0] != "https://vimeo.com/12345" {
		t.Fatalf("resolved queries = %v", q)
	}
	// A repeat of a non-YouTube URL may resolve again (the catalog keeps the
	// canonical page URL), but the stored file is never fetched twice.
	if w := e.do("GET", filesPath("https://vimeo.com/12345", "", "wav"), e.user, ""); w.Code != 200 {
		t.Fatalf("repeat: %d", w.Code)
	}
	if _, f := e.fake.counts(media.Key(fakeID("https://vimeo.com/12345"), media.WAV)); f != 1 {
		t.Fatalf("fetched %d times", f)
	}
}

func TestBadRequests(t *testing.T) {
	e := newEnv(t, envOpts{})
	for path, want := range map[string]int{
		"/v1/files?format=mp3":                                400,
		filesPath(ytURL, "", "flac"):                          400,
		filesPath("ftp://x/y", "", "mp3"):                     400,
		filesPath("https://unavailable.example/x", "", "mp3"): 502,
		"/v1/tasks/not-a-uuid":                                400,
		"/v1/tasks/00000000-0000-0000-0000-000000000000":      404,
		"/v1/history?sort=title":                              400,
		"/v1/history?page_size=1000":                          400,
		"/v1/history?from=yesterday":                          400,
	} {
		if w := e.do("GET", path, e.user, ""); w.Code != want {
			t.Errorf("GET %s: %d, want %d (%s)", path, w.Code, want, w.Body.String())
		} else if !strings.Contains(w.Body.String(), `"error"`) {
			t.Errorf("GET %s: body is not a JSON error: %s", path, w.Body.String())
		}
	}
}

func TestRangeAndConditional(t *testing.T) {
	e := newEnv(t, envOpts{})
	full := fileBytes("dQw4w9WgXcQ", media.MP4)
	w := e.do("GET", filesPath(ytURL, "", "mp4"), e.user, "")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	etag := w.Header().Get("ETag")
	if etag == "" || w.Header().Get("Accept-Ranges") != "bytes" {
		t.Fatalf("headers: %v", w.Header())
	}

	w = e.do("GET", filesPath(ytURL, "", "mp4"), e.user, "", "Range", "bytes=10-19")
	if w.Code != 206 || !bytes.Equal(w.Body.Bytes(), full[10:20]) {
		t.Fatalf("range: %d %q", w.Code, w.Body.String())
	}
	if cr := w.Header().Get("Content-Range"); cr != fmt.Sprintf("bytes 10-19/%d", len(full)) {
		t.Fatalf("Content-Range = %q", cr)
	}
	w = e.do("GET", filesPath(ytURL, "", "mp4"), e.user, "", "Range", "bytes=-5")
	if w.Code != 206 || !bytes.Equal(w.Body.Bytes(), full[len(full)-5:]) {
		t.Fatalf("suffix range: %d %q", w.Code, w.Body.String())
	}
	if w := e.do("GET", filesPath(ytURL, "", "mp4"), e.user, "", "Range", fmt.Sprintf("bytes=%d-", len(full)+10)); w.Code != 416 {
		t.Fatalf("unsatisfiable range: %d", w.Code)
	}
	if w := e.do("GET", filesPath(ytURL, "", "mp4"), e.user, "", "If-None-Match", etag); w.Code != 304 || w.Body.Len() != 0 {
		t.Fatalf("If-None-Match: %d", w.Code)
	}
	if w := e.do("HEAD", filesPath(ytURL, "", "mp4"), e.user, ""); w.Code != 200 || w.Body.Len() != 0 ||
		w.Header().Get("Content-Length") != strconv.Itoa(len(full)) {
		t.Fatalf("HEAD: %d len=%d cl=%s", w.Code, w.Body.Len(), w.Header().Get("Content-Length"))
	}
}

func TestStream(t *testing.T) {
	e := newEnv(t, envOpts{})
	path := strings.Replace(filesPath(ytURL, "", "mp3"), "/v1/files", "/v1/stream", 1)

	w := e.do("GET", path, e.user, "")
	if w.Code != 200 || w.Body.String() != string(liveBytes("dQw4w9WgXcQ", media.MP3)) {
		t.Fatalf("live stream: %d %q", w.Code, w.Body.String())
	}
	if w.Header().Get("Content-Type") != "audio/mpeg" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("live headers: %v", w.Header())
	}
	if _, f := e.fake.counts(media.Key("dQw4w9WgXcQ", media.MP3)); f != 0 {
		t.Fatal("live stream stored the file")
	}

	e.do("GET", filesPath(ytURL, "", "mp3"), e.user, "")
	w = e.do("GET", path, e.user, "", "Range", "bytes=0-3")
	if w.Code != 206 || w.Body.String() != "dQw4" || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "inline") {
		t.Fatalf("stored stream: %d %q %v", w.Code, w.Body.String(), w.Header())
	}
}

func TestTaskLifecycleAndIdempotency(t *testing.T) {
	e := newEnv(t, envOpts{resultTTL: 2 * time.Second})
	body := `{"url":"` + ytURL + `","format":"wav"}`

	created := e.json(e.do("POST", "/v1/tasks", e.user, body, "Idempotency-Key", "k-1"), 202)
	id := created["id"].(string)
	if created["status"] != "queued" || created["status_url"] != "/v1/tasks/"+id {
		t.Fatalf("created: %v", created)
	}

	// Same key, same body: the same task, not a new one.
	again := e.json(e.do("POST", "/v1/tasks", e.user, body, "Idempotency-Key", "k-1"), 200)
	if again["id"] != id {
		t.Fatalf("replay returned another task: %v", again)
	}
	// Same key, different request: rejected.
	if w := e.do("POST", "/v1/tasks", e.user, `{"url":"`+ytURL+`","format":"mp3"}`, "Idempotency-Key", "k-1"); w.Code != 422 {
		t.Fatalf("key reuse with different body: %d", w.Code)
	}
	// Another user's identical key is independent.
	if w := e.do("POST", "/v1/tasks", e.admin, body, "Idempotency-Key", "k-1"); w.Code != 202 {
		t.Fatalf("other user's key: %d", w.Code)
	}

	done := e.waitTask(e.user, id, finished)
	if done["status"] != "succeeded" || done["file_available"] != true || done["file_url"] != "/v1/tasks/"+id+"/file" {
		t.Fatalf("finished task: %v", done)
	}

	// Without a key, an identical request reuses the finished task while its
	// result is downloadable, instead of starting another download.
	implicit := e.json(e.do("POST", "/v1/tasks", e.user, body), 200)
	if implicit["id"] != id {
		t.Fatalf("implicit idempotency returned task %v, want %v", implicit["id"], id)
	}
	// A different request (other format) is a new task.
	if w := e.do("POST", "/v1/tasks", e.user, `{"url":"`+ytURL+`","format":"mp4"}`); w.Code != 202 {
		t.Fatalf("different request: %d", w.Code)
	}

	w := e.do("GET", "/v1/tasks/"+id+"/file", e.user, "")
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), fileBytes("dQw4w9WgXcQ", media.WAV)) {
		t.Fatalf("task file: %d", w.Code)
	}
	// Other users cannot see the task at all.
	other := e.json(e.do("POST", "/v1/users", e.admin, `{"username":"carol","email":"carol@example.com"}`), 201)
	carol := other["api_key"].(map[string]any)["key"].(string)
	if w := e.do("GET", "/v1/tasks/"+id, carol, ""); w.Code != 404 {
		t.Fatalf("foreign task: %d", w.Code)
	}
	if w := e.do("GET", "/v1/tasks/"+id, e.admin, ""); w.Code != 200 {
		t.Fatalf("admin view of task: %d", w.Code)
	}

	time.Sleep(2100 * time.Millisecond)
	if w := e.do("GET", "/v1/tasks/"+id+"/file", e.user, ""); w.Code != 410 {
		t.Fatalf("expired task file: %d %s", w.Code, w.Body.String())
	}

	// Failed tasks report why and have no file.
	failed := e.json(e.do("POST", "/v1/tasks", e.user, `{"url":"https://unavailable.example/x","format":"mp3"}`), 202)
	f := e.waitTask(e.user, failed["id"].(string), finished)
	if f["status"] != "failed" || f["error"] == nil {
		t.Fatalf("failed task: %v", f)
	}
	if w := e.do("GET", "/v1/tasks/"+failed["id"].(string)+"/file", e.user, ""); w.Code != 409 {
		t.Fatalf("failed task file: %d", w.Code)
	}
}

func TestWebhookDelivery(t *testing.T) {
	e := newEnv(t, envOpts{})
	secret := e.json(e.do("GET", "/v1/me/webhook-secret", e.user, ""), 200)["webhook_secret"].(string)

	type delivery struct {
		event, task, sig, sum string
		body                  []byte
		err                   error
	}
	got := make(chan delivery, 4)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sum := sha256.Sum256(b)
		d := delivery{
			event: r.Header.Get(jobs.HeaderEvent), task: r.Header.Get(jobs.HeaderTaskID),
			sig: r.Header.Get(jobs.HeaderSignature), sum: hex.EncodeToString(sum[:]), body: b,
		}
		if d.sum != r.Header.Get(jobs.HeaderContentSHA256) {
			d.err = fmt.Errorf("content hash header mismatch")
		} else {
			d.err = jobs.VerifySignature(secret, d.sig, d.task, d.sum, 5*time.Minute, time.Now())
		}
		got <- d
	}))
	defer receiver.Close()

	created := e.json(e.do("POST", "/v1/tasks", e.user, `{"url":"`+ytURL+`","format":"mp3","webhook_url":"`+receiver.URL+`/hook"}`), 202)
	select {
	case d := <-got:
		if d.err != nil {
			t.Fatalf("signature check: %v", d.err)
		}
		if d.event != jobs.EventSucceeded || d.task != created["id"] || !bytes.Equal(d.body, fileBytes("dQw4w9WgXcQ", media.MP3)) {
			t.Fatalf("delivery: event=%s task=%s len=%d", d.event, d.task, len(d.body))
		}
	case <-time.After(15 * time.Second):
		t.Fatal("webhook not delivered")
	}
	e.waitTask(e.user, created["id"].(string), func(m map[string]any) bool { return m["webhook_state"] == "delivered" })

	// Failed tasks notify too, with a signed JSON event.
	failed := e.json(e.do("POST", "/v1/tasks", e.user, `{"url":"https://unavailable.example/x","format":"mp3","webhook_url":"`+receiver.URL+`/hook"}`), 202)
	select {
	case d := <-got:
		if d.err != nil || d.event != jobs.EventFailed || d.task != failed["id"] || !strings.Contains(string(d.body), `"status":"failed"`) {
			t.Fatalf("failure delivery: %+v body=%s", d, d.body)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("failure webhook not delivered")
	}

	// Rotating the secret changes what signatures verify against.
	rotated := e.json(e.do("POST", "/v1/me/webhook-secret/rotate", e.user, ""), 200)["webhook_secret"].(string)
	if rotated == secret || len(rotated) != 64 {
		t.Fatalf("rotated secret: %q", rotated)
	}

	if w := e.do("POST", "/v1/tasks", e.user, `{"url":"`+ytURL+`","format":"mp3","webhook_url":"ftp://x"}`); w.Code != 400 {
		t.Fatalf("bad webhook scheme: %d", w.Code)
	}
}

func TestWebhookGivesUp(t *testing.T) {
	e := newEnv(t, envOpts{webhookMaxAttempts: 1})
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer receiver.Close()
	created := e.json(e.do("POST", "/v1/tasks", e.user, `{"url":"`+ytURL+`","format":"mp3","webhook_url":"`+receiver.URL+`"}`), 202)
	task := e.waitTask(e.user, created["id"].(string), func(m map[string]any) bool { return m["webhook_state"] == "failed" })
	if task["status"] != "succeeded" || task["webhook_error"] == nil || task["file_available"] != true {
		t.Fatalf("task after webhook failure: %v", task)
	}
}

func TestHistoryFiltersSortingPaging(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.do("GET", filesPath(ytURL, "", "mp3"), e.user, "")
	e.do("GET", filesPath(ytURL, "", "mp4"), e.user, "")
	e.do("GET", strings.Replace(filesPath("", "some song", "wav"), "/v1/files", "/v1/stream", 1), e.user, "")
	e.do("GET", filesPath("https://unavailable.example/x", "", "mp3"), e.user, "")
	e.do("GET", filesPath(ytURL, "", "mp3"), e.admin, "")

	page := func(key, q string) ([]map[string]any, int64) {
		t.Helper()
		m := e.json(e.do("GET", "/v1/history?"+q, key, ""), 200)
		var items []map[string]any
		for _, it := range m["items"].([]any) {
			items = append(items, it.(map[string]any))
		}
		return items, intOf(m["total"])
	}

	all, total := page(e.user, "")
	if total != 4 || len(all) != 4 {
		t.Fatalf("own history: %d items, total %d", len(all), total)
	}
	for _, it := range all {
		if intOf(it["user_id"]) != e.userID {
			t.Fatalf("user sees someone else's row: %v", it)
		}
	}
	if all[0]["status"] != "error" || all[0]["url"] != "https://unavailable.example/x" {
		t.Fatalf("default sort is not newest first: %v", all[0])
	}
	asc, _ := page(e.user, "sort=created_at")
	if asc[0]["format"] != "mp3" || asc[3]["status"] != "error" {
		t.Fatalf("ascending sort: first=%v last=%v", asc[0], asc[3])
	}
	if items, n := page(e.user, "kind=stream"); n != 1 || items[0]["query"] != "some song" {
		t.Fatalf("kind filter: %d %v", n, items)
	}
	if _, n := page(e.user, "format=mp3"); n != 2 {
		t.Fatalf("format filter: %d", n)
	}
	if _, n := page(e.user, "status=error"); n != 1 {
		t.Fatalf("status filter: %d", n)
	}
	if _, n := page(e.user, "q=SOME"); n != 1 {
		t.Fatalf("search: %d", n)
	}
	if _, n := page(e.user, "q=%25"); n != 0 {
		t.Fatalf("LIKE wildcard not escaped: %d", n)
	}
	if items, n := page(e.user, "page=2&page_size=3"); n != 4 || len(items) != 1 {
		t.Fatalf("paging: total %d, %d items", n, len(items))
	}
	tomorrow := time.Now().Add(24 * time.Hour).Format(time.DateOnly)
	if _, n := page(e.user, "from="+tomorrow); n != 0 {
		t.Fatalf("from filter: %d", n)
	}
	if _, n := page(e.user, "to="+tomorrow); n != 4 {
		t.Fatalf("to filter: %d", n)
	}

	if w := e.do("GET", fmt.Sprintf("/v1/history?user_id=%d", e.adminID), e.user, ""); w.Code != 403 {
		t.Fatalf("user reading admin history: %d", w.Code)
	}
	if _, n := page(e.admin, ""); n != 5 {
		t.Fatalf("admin sees all history: %d", n)
	}
	if _, n := page(e.admin, fmt.Sprintf("user_id=%d", e.userID)); n != 4 {
		t.Fatalf("admin user filter: %d", n)
	}
}

func TestRateLimit(t *testing.T) {
	e := newEnv(t, envOpts{rateLimit: 3, burst: 3})
	var last *httptest.ResponseRecorder
	for i := 0; i < 4; i++ {
		last = e.do("GET", "/v1/me", e.user, "")
	}
	if last.Code != 429 || last.Header().Get("Retry-After") == "" {
		t.Fatalf("4th request: %d, Retry-After=%q", last.Code, last.Header().Get("Retry-After"))
	}
	// Limits are per key: the admin key is unaffected.
	if w := e.do("GET", "/v1/me", e.admin, ""); w.Code == 429 {
		t.Fatal("rate limit leaked across keys")
	}
}

func TestReadCache(t *testing.T) {
	e := newEnv(t, envOpts{})
	if h := e.do("GET", "/v1/users", e.admin, "").Header().Get("X-Cache"); h != "MISS" {
		t.Fatalf("first read: %s", h)
	}
	w := e.do("GET", "/v1/users", e.admin, "")
	if w.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("second read: %s", w.Header().Get("X-Cache"))
	}
	e.json(e.do("POST", "/v1/users", e.admin, `{"username":"dave","email":"dave@example.com"}`), 201)
	w = e.do("GET", "/v1/users", e.admin, "")
	if w.Header().Get("X-Cache") != "MISS" || !strings.Contains(w.Body.String(), "dave") {
		t.Fatalf("after write: %s %s", w.Header().Get("X-Cache"), w.Body.String())
	}
	// Different callers never share entries.
	if h := e.do("GET", "/v1/me", e.user, "").Header().Get("X-Cache"); h != "MISS" {
		t.Fatalf("user /me: %s", h)
	}
	if !strings.Contains(e.do("GET", "/v1/me", e.user, "").Body.String(), "alice") {
		t.Fatal("cached /me served the wrong user")
	}
}

func TestUserCRUDAndKeys(t *testing.T) {
	e := newEnv(t, envOpts{})
	res := e.json(e.do("POST", "/v1/users", e.admin, `{"username":"erin","email":"erin@example.com","role":"user","key_name":"cli"}`), 201)
	erinID := intOf(res["user"].(map[string]any)["id"])
	erinKey := res["api_key"].(map[string]any)["key"].(string)
	keyID := intOf(res["api_key"].(map[string]any)["id"])
	if w := e.do("GET", "/v1/me", erinKey, ""); w.Code != 200 {
		t.Fatalf("new user's key: %d", w.Code)
	}

	for body, want := range map[string]int{
		`{"username":"erin","email":"x@example.com"}`:                 409, // duplicate username
		`{"username":"erin2","email":"ERIN@example.com"}`:             409, // duplicate email (case-insensitive)
		`{"username":"e","email":"e@example.com"}`:                    400,
		`{"username":"erin3","email":"not-an-email"}`:                 400,
		`{"username":"erin4","email":"e4@example.com","role":"root"}`: 400,
	} {
		if w := e.do("POST", "/v1/users", e.admin, body); w.Code != want {
			t.Errorf("create %s: %d, want %d", body, w.Code, want)
		}
	}

	u := e.json(e.do("PATCH", fmt.Sprintf("/v1/users/%d", erinID), e.admin, `{"role":"admin"}`), 200)
	if u["role"] != "admin" || u["username"] != "erin" {
		t.Fatalf("patch: %v", u)
	}
	if w := e.do("PATCH", fmt.Sprintf("/v1/users/%d", e.adminID), e.admin, `{"role":"user"}`); w.Code != 409 {
		t.Fatalf("self-demotion: %d", w.Code)
	}

	// Refresh: the old key stops working, the new one works.
	ref := e.json(e.do("POST", fmt.Sprintf("/v1/users/%d/api-keys/%d/refresh", erinID, keyID), e.admin, ""), 200)
	newKey := ref["key"].(string)
	if ref["name"] != "cli" {
		t.Fatalf("refreshed key name: %v", ref)
	}
	if w := e.do("GET", "/v1/me", erinKey, ""); w.Code != 401 {
		t.Fatalf("old key after refresh: %d", w.Code)
	}
	if w := e.do("GET", "/v1/me", newKey, ""); w.Code != 200 {
		t.Fatalf("new key: %d", w.Code)
	}
	keys := e.json(e.do("GET", fmt.Sprintf("/v1/users/%d/api-keys", erinID), e.admin, ""), 200)["items"].([]any)
	if len(keys) != 2 || strings.Contains(fmt.Sprint(keys), "hash") {
		t.Fatalf("key listing: %v", keys)
	}

	// Single revoke, then revoke-all.
	second := e.json(e.do("POST", fmt.Sprintf("/v1/users/%d/api-keys", erinID), e.admin, `{"name":"ci"}`), 201)
	if w := e.do("POST", fmt.Sprintf("/v1/users/%d/api-keys/%d/revoke", erinID, intOf(second["id"])), e.admin, ""); w.Code != 204 {
		t.Fatalf("revoke: %d", w.Code)
	}
	if w := e.do("GET", "/v1/me", second["key"].(string), ""); w.Code != 401 {
		t.Fatalf("revoked key still works: %d", w.Code)
	}
	all := e.json(e.do("POST", fmt.Sprintf("/v1/users/%d/api-keys/revoke-all", erinID), e.admin, ""), 200)
	if intOf(all["revoked"]) != 1 {
		t.Fatalf("revoke-all: %v", all)
	}
	if w := e.do("GET", "/v1/me", newKey, ""); w.Code != 401 {
		t.Fatalf("key after revoke-all: %d", w.Code)
	}

	if w := e.do("DELETE", fmt.Sprintf("/v1/users/%d", e.adminID), e.admin, ""); w.Code != 409 {
		t.Fatalf("self-delete: %d", w.Code)
	}
	if w := e.do("DELETE", fmt.Sprintf("/v1/users/%d", erinID), e.admin, ""); w.Code != 204 {
		t.Fatalf("delete: %d", w.Code)
	}
	if w := e.do("GET", fmt.Sprintf("/v1/users/%d", erinID), e.admin, ""); w.Code != 404 {
		t.Fatalf("deleted user: %d", w.Code)
	}
}

func TestCatalogDelete(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.do("GET", filesPath(ytURL, "", "mp4"), e.user, "")
	list := e.json(e.do("GET", "/v1/videos", e.admin, ""), 200)
	if intOf(list["total"]) != 1 {
		t.Fatalf("videos: %v", list)
	}
	if w := e.do("DELETE", "/v1/videos/dQw4w9WgXcQ", e.user, ""); w.Code != 403 {
		t.Fatalf("user delete: %d", w.Code)
	}
	if w := e.do("DELETE", "/v1/videos/dQw4w9WgXcQ", e.admin, ""); w.Code != 204 {
		t.Fatalf("admin delete: %d", w.Code)
	}
	if _, err := e.store.Stat(context.Background(), media.Key("dQw4w9WgXcQ", media.MP4)); err == nil {
		t.Fatal("stored file survived catalog delete")
	}
	if w := e.do("GET", "/v1/videos/dQw4w9WgXcQ", e.admin, ""); w.Code != 404 {
		t.Fatalf("deleted row: %d", w.Code)
	}
}

func TestCleanupEvictsIdleFiles(t *testing.T) {
	e := newEnv(t, envOpts{})
	ctx := context.Background()
	idle := "https://www.youtube.com/watch?v=aaaaaaaaaaa"
	pinned := "https://www.youtube.com/watch?v=bbbbbbbbbbb"
	fresh := "https://www.youtube.com/watch?v=ccccccccccc"

	e.do("GET", filesPath(idle, "", "mp3"), e.user, "")
	e.do("GET", filesPath(fresh, "", "mp3"), e.user, "")
	// A task whose result is still downloadable pins its file.
	task := e.json(e.do("POST", "/v1/tasks", e.user, `{"url":"`+pinned+`","format":"mp3"}`), 202)
	e.waitTask(e.user, task["id"].(string), finished)

	if _, err := e.db.Pool.Exec(ctx,
		`UPDATE audio SET last_requested_at = now() - interval '30 days' WHERE source_id IN ('aaaaaaaaaaa', 'bbbbbbbbbbb')`); err != nil {
		t.Fatal(err)
	}
	n, err := e.runner.CleanupOnce(ctx)
	if err != nil || n != 1 {
		t.Fatalf("cleanup removed %d, err %v", n, err)
	}

	stored := func(id string) bool {
		_, err := e.store.Stat(ctx, media.Key(id, media.MP3))
		return err == nil
	}
	if stored("aaaaaaaaaaa") || !stored("bbbbbbbbbbb") || !stored("ccccccccccc") {
		t.Fatalf("after cleanup: idle=%v pinned=%v fresh=%v", stored("aaaaaaaaaaa"), stored("bbbbbbbbbbb"), stored("ccccccccccc"))
	}
	row := e.json(e.do("GET", "/v1/audio/aaaaaaaaaaa/mp3", e.admin, ""), 200)
	if row["stored"] != false || row["storage_key"] != nil {
		t.Fatalf("catalog row after eviction: %v", row)
	}

	// Requesting an evicted file downloads it again.
	_, before := e.fake.counts(media.Key("aaaaaaaaaaa", media.MP3))
	if w := e.do("GET", filesPath(idle, "", "mp3"), e.user, ""); w.Code != 200 {
		t.Fatalf("re-request: %d", w.Code)
	}
	if _, after := e.fake.counts(media.Key("aaaaaaaaaaa", media.MP3)); after != before+1 {
		t.Fatalf("evicted file not re-fetched: %d -> %d", before, after)
	}
}

func TestHealth(t *testing.T) {
	e := newEnv(t, envOpts{})
	if w := e.do("GET", "/healthz", "", ""); w.Code != 200 {
		t.Fatalf("healthz: %d", w.Code)
	}
	if m := e.json(e.do("GET", "/readyz", "", ""), 200); m["database"] != "ok" || m["redis"] != "ok" {
		t.Fatalf("readyz: %v", m)
	}
}
