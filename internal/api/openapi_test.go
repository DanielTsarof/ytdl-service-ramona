package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/gin-gonic/gin"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/api"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/app"
)

// The spec is the HTTP contract. These tests keep it honest:
//   - it is a valid OpenAPI document;
//   - every Gin route is documented and every documented operation exists;
//   - every response produced by the integration tests (api_test.go)
//     conforms to it (see validateResponse, called from env.do).

var (
	specOnce   sync.Once
	specDoc    *openapi3.T
	specRouter routers.Router
	specErr    error
)

func loadSpec(t testing.TB) (*openapi3.T, routers.Router) {
	t.Helper()
	specOnce.Do(func() {
		raw, err := os.ReadFile("openapi.yaml")
		if err != nil {
			specErr = err
			return
		}
		loader := openapi3.NewLoader()
		specDoc, err = loader.LoadFromData(raw)
		if err != nil {
			specErr = fmt.Errorf("load: %w", err)
			return
		}
		if err := specDoc.Validate(loader.Context); err != nil {
			specErr = fmt.Errorf("validate: %w", err)
			return
		}
		specRouter, specErr = gorillamux.NewRouter(specDoc)

		// Media and document bodies are opaque to the validator: it only
		// checks the content type is one the operation declares.
		for _, ct := range []string{"audio/mpeg", "audio/wav", "video/mp4"} {
			openapi3filter.RegisterBodyDecoder(ct, openapi3filter.FileBodyDecoder)
		}
		for _, ct := range []string{"application/yaml", "text/html"} {
			openapi3filter.RegisterBodyDecoder(ct, func(r io.Reader, _ http.Header, _ *openapi3.SchemaRef, _ openapi3filter.EncodingFn) (any, error) {
				b, err := io.ReadAll(r)
				return string(b), err
			})
		}
	})
	if specErr != nil {
		t.Fatalf("openapi.yaml: %v", specErr)
	}
	return specDoc, specRouter
}

func TestSpecIsValid(t *testing.T) {
	doc, _ := loadSpec(t)
	ids := map[string]string{}
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if op.OperationID == "" {
				t.Errorf("%s %s has no operationId", method, path)
			}
			if prev, dup := ids[op.OperationID]; dup {
				t.Errorf("operationId %q used by %s and %s %s", op.OperationID, prev, method, path)
			}
			ids[op.OperationID] = method + " " + path
		}
	}
}

var ginParam = regexp.MustCompile(`:([A-Za-z_]+)`)

// TestRouteParity fails when a route is added without documenting it, or the
// spec documents an operation the router does not have.
func TestRouteParity(t *testing.T) {
	doc, _ := loadSpec(t)
	srv := api.New(app.New(nil, nil, nil, nil, discard), nil, nil, api.Config{}, discard)
	engine, ok := srv.Handler().(*gin.Engine)
	if !ok {
		t.Fatal("Handler is not a *gin.Engine")
	}

	routed := map[string]bool{}
	for _, r := range engine.Routes() {
		routed[r.Method+" "+ginParam.ReplaceAllString(r.Path, "{$1}")] = true
	}
	documented := map[string]bool{}
	for path, item := range doc.Paths.Map() {
		for method := range item.Operations() {
			documented[strings.ToUpper(method)+" "+path] = true
		}
	}

	var undocumented, unrouted []string
	for k := range routed {
		if !documented[k] {
			undocumented = append(undocumented, k)
		}
	}
	for k := range documented {
		if !routed[k] {
			unrouted = append(unrouted, k)
		}
	}
	sort.Strings(undocumented)
	sort.Strings(unrouted)
	if len(undocumented) > 0 {
		t.Errorf("routes missing from openapi.yaml:\n  %s", strings.Join(undocumented, "\n  "))
	}
	if len(unrouted) > 0 {
		t.Errorf("operations in openapi.yaml without a route:\n  %s", strings.Join(unrouted, "\n  "))
	}
}

// validateResponse checks one recorded response against the operation the
// request matched. Called by env.do for every request in the API tests.
func validateResponse(t testing.TB, req *http.Request, w *httptest.ResponseRecorder) {
	t.Helper()
	_, router := loadSpec(t)
	route, params, err := router.FindRoute(req)
	if err != nil {
		t.Errorf("%s %s: not in openapi.yaml: %v", req.Method, req.URL.Path, err)
		return
	}
	input := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route},
		Status:                 w.Code,
		Header:                 w.Header(),
		Body:                   io.NopCloser(bytes.NewReader(w.Body.Bytes())),
		Options:                &openapi3filter.Options{IncludeResponseStatus: true},
	}
	if err := openapi3filter.ValidateResponse(context.Background(), input); err != nil {
		body := w.Body.String()
		if len(body) > 300 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
			body = fmt.Sprintf("<%d bytes of %s>", w.Body.Len(), w.Header().Get("Content-Type"))
		}
		t.Errorf("%s %s -> %d does not match openapi.yaml: %v\nbody: %s", req.Method, req.URL.Path, w.Code, err, body)
	}
}

// The docs endpoints need no database or Redis.
func TestDocsEndpoints(t *testing.T) {
	doc, _ := loadSpec(t)
	h := api.New(app.New(nil, nil, nil, nil, discard), nil, nil, api.Config{}, discard).Handler()
	get := func(path string, hdr ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		validateResponse(t, req, w)
		return w
	}

	w := get("/openapi.json")
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("/openapi.json: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	var js struct {
		OpenAPI string                    `json:"openapi"`
		Paths   map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &js); err != nil {
		t.Fatalf("/openapi.json is not JSON: %v", err)
	}
	if js.OpenAPI != doc.OpenAPI || len(js.Paths) != doc.Paths.Len() {
		t.Fatalf("JSON spec differs: openapi=%s paths=%d (yaml %d)", js.OpenAPI, len(js.Paths), doc.Paths.Len())
	}
	if w2 := get("/openapi.json", "If-None-Match", w.Header().Get("ETag")); w2.Code != 304 {
		t.Fatalf("conditional GET: %d", w2.Code)
	}

	y := get("/openapi.yaml")
	if y.Code != 200 || !bytes.HasPrefix(y.Body.Bytes(), []byte("openapi: 3.0.3")) {
		t.Fatalf("/openapi.yaml: %d", y.Code)
	}

	d := get("/docs")
	csp := d.Header().Get("Content-Security-Policy")
	if d.Code != 200 || !strings.Contains(d.Body.String(), "swagger-ui-dist@5.33.1/swagger-ui-bundle.js") ||
		!strings.Contains(d.Body.String(), `integrity="sha384-`) || !strings.Contains(csp, "script-src 'nonce-") {
		t.Fatalf("/docs: %d csp=%q", d.Code, csp)
	}
	// The nonce in the page matches the header and changes per request.
	nonce := regexp.MustCompile(`'nonce-([^']+)'`).FindStringSubmatch(csp)[1]
	if !strings.Contains(d.Body.String(), `nonce="`+nonce+`"`) {
		t.Fatal("page nonce does not match CSP")
	}
	if get("/docs").Header().Get("Content-Security-Policy") == csp {
		t.Fatal("nonce reused across requests")
	}
}
