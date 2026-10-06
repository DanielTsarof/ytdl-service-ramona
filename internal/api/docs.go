package api

import (
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

// openapiYAML is the API contract. It is hand-written; openapi_test.go keeps
// it in step with the router (every route documented, every documented
// operation routed) and the integration tests validate real responses
// against it.
//
//go:embed openapi.yaml
var openapiYAML []byte

// spec holds the document in both encodings, prepared once.
type spec struct {
	yaml, json []byte
	etag       string
}

func loadSpec() (spec, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(openapiYAML, &doc); err != nil {
		return spec{}, fmt.Errorf("openapi.yaml: %w", err)
	}
	js, err := json.Marshal(doc)
	if err != nil {
		return spec{}, fmt.Errorf("openapi.yaml to JSON: %w", err)
	}
	sum := sha256.Sum256(openapiYAML)
	return spec{yaml: openapiYAML, json: js, etag: `"` + hex.EncodeToString(sum[:12]) + `"`}, nil
}

// mustSpec panics on a malformed embedded spec: that is a build defect, and
// the spec tests catch it before it ships.
func mustSpec() spec {
	s, err := loadSpec()
	if err != nil {
		panic(err)
	}
	return s
}

func (sp spec) serve(contentType string, body []byte) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("ETag", sp.etag)
		c.Header("Cache-Control", "public, max-age=300")
		if match := c.GetHeader("If-None-Match"); match == sp.etag {
			c.Status(http.StatusNotModified)
			return
		}
		c.Data(http.StatusOK, contentType, body)
	}
}

// Swagger UI is loaded from jsDelivr at a pinned version with SRI hashes, so
// the binary does not carry ~2 MB of assets and a tampered CDN file is
// refused by the browser.
const (
	swaggerUIVersion = "5.33.1"
	swaggerUIBase    = "https://cdn.jsdelivr.net/npm/swagger-ui-dist@" + swaggerUIVersion
	swaggerCSSSRI    = "sha384-Ov4/wv3j2bmct8cDc5X4ngJZohVPzEmc6uDPH8WeljUxO5vtoykvMEfbu9Vh6RaW"
	swaggerJSSRI     = "sha384-ZPehFMQommnnuaZ4rpxgkgTT2DKFVp4hZC/7pLit+9Lek9T1YGSo23eHFbvNkXkw"
)

var docsPage = template.Must(template.New("docs").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>ytdl-service API</title>
<link rel="stylesheet" href="{{.Base}}/swagger-ui.css" integrity="{{.CSSSRI}}" crossorigin="anonymous">
<style>body{margin:0}</style>
</head>
<body>
<div id="swagger-ui"></div>
<script src="{{.Base}}/swagger-ui-bundle.js" integrity="{{.JSSRI}}" crossorigin="anonymous"></script>
<script nonce="{{.Nonce}}">
window.ui = SwaggerUIBundle({
  url: "/openapi.json",
  dom_id: "#swagger-ui",
  deepLinking: true,
  persistAuthorization: true,
  tryItOutEnabled: true,
  displayRequestDuration: true,
  validatorUrl: null
});
</script>
</body>
</html>
`))

// docs serves the interactive documentation. A per-request nonce lets the
// one inline script run under a CSP that otherwise allows scripts only from
// the pinned CDN. Styles allow 'unsafe-inline': Swagger UI sets inline
// styles, and a style nonce would make browsers ignore 'unsafe-inline'.
func (s *Server) docs(c *gin.Context) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		s.fail(c, err)
		return
	}
	// base64url: valid in a CSP nonce and never escaped by html/template
	// (standard base64's "+" would be written as "&#43;" in the attribute).
	nonce := base64.RawURLEncoding.EncodeToString(b)
	c.Header("Content-Security-Policy", "default-src 'none'; "+
		"script-src 'nonce-"+nonce+"' https://cdn.jsdelivr.net; "+
		"style-src 'unsafe-inline' https://cdn.jsdelivr.net; "+
		"img-src 'self' data: https://cdn.jsdelivr.net; "+
		"connect-src 'self'; "+
		"font-src https://cdn.jsdelivr.net; "+
		"base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Status(http.StatusOK)
	if err := docsPage.Execute(c.Writer, map[string]string{
		"Base": swaggerUIBase, "CSSSRI": swaggerCSSSRI, "JSSRI": swaggerJSSRI, "Nonce": nonce,
	}); err != nil {
		s.log.Warn("rendering docs page failed", "err", err)
	}
}
