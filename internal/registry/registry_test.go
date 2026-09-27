package registry_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"onegit/internal/auth"
	"onegit/internal/blob"
	"onegit/internal/config"
	"onegit/internal/registry"
	"onegit/internal/store"
	"onegit/internal/testutil"
)

var ctx = context.Background()

type env struct {
	t      *testing.T
	cfg    *config.Config
	st     *store.Store
	blobs  *blob.Store
	auth   *auth.Service
	svc    *registry.Service
	srv    *httptest.Server
	writer *store.User
	reader *store.User
}

func setup(t *testing.T, tweak func(*config.Config)) *env {
	t.Helper()
	cfg := testutil.Config(t)
	if tweak != nil {
		tweak(cfg)
	}
	e := &env{t: t, cfg: cfg, st: testutil.Store(t, cfg), blobs: testutil.Blob(t, cfg)}
	kvs := testutil.KV(t, cfg)
	e.auth = &auth.Service{Store: e.st, KV: kvs}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case registry.Match(r):
			e.svc.ServeHTTP(w, r)
		case registry.MatchAPI(r):
			e.svc.ServeAPI(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(e.srv.Close)
	cfg.HTTP.BaseURL = e.srv.URL
	var err error
	if e.svc, err = registry.New(ctx, e.st, e.blobs, kvs, e.auth, cfg, testutil.Logger()); err != nil {
		t.Fatal(err)
	}
	e.writer = testutil.User(t, e.st, "writer", store.RoleWrite)
	e.reader = testutil.User(t, e.st, "reader", store.RoleRead)
	return e
}

// client speaks the distribution API with a bearer token.
type client struct {
	e     *env
	token string
}

func (e *env) login(user, password string) *client {
	e.t.Helper()
	req, _ := http.NewRequest("GET", e.srv.URL+"/v2/token?service=x&scope=repository:a:pull", nil)
	if user != "" {
		req.SetBasicAuth(user, password)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Token     string `json:"token"`
		ExpiresIn int    `json:"expires_in"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(body.Token, "ogr.") || body.ExpiresIn != 900 {
		e.t.Fatalf("token for %s: %d %+v", user, resp.StatusCode, body)
	}
	return &client{e: e, token: body.Token}
}

type result struct {
	*http.Response
	body []byte
}

func (r result) code() string {
	var errs struct {
		Errors []struct{ Code string } `json:"errors"`
	}
	json.Unmarshal(r.body, &errs)
	if len(errs.Errors) == 0 {
		return ""
	}
	return errs.Errors[0].Code
}

func (c *client) do(method, path string, body []byte, headers ...string) result {
	c.e.t.Helper()
	u := path
	if !strings.HasPrefix(u, "http") {
		u = c.e.srv.URL + path
	}
	req, _ := http.NewRequest(method, u, bytes.NewReader(body))
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return result{resp, b}
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// pushBlob uploads b monolithically and returns its digest.
func (c *client) pushBlob(name string, b []byte) string {
	c.e.t.Helper()
	d := digestOf(b)
	r := c.do("POST", "/v2/"+name+"/blobs/uploads/?digest="+d, b, "Content-Type", "application/octet-stream")
	if r.StatusCode != http.StatusCreated || r.Header.Get("Docker-Content-Digest") != d {
		c.e.t.Fatalf("push blob: %d %s", r.StatusCode, r.body)
	}
	return d
}

// image builds and pushes a single-layer image, returning the manifest.
func (c *client) image(name, tag, layer string) (manifest []byte, digest string) {
	c.e.t.Helper()
	config := []byte(`{"architecture":"arm64","os":"linux","variant":"v8","config":{"Labels":{"org.opencontainers.image.source":"x"},` +
		`"Env":["PATH=/bin"],"Cmd":["/app"],"ExposedPorts":{"8080/tcp":{}}},"history":[` +
		`{"created_by":"/bin/sh -c #(nop) ADD file:abc in / "},{"created_by":"/bin/sh -c #(nop)  CMD [\"/app\"]","empty_layer":true}]}`)
	cd := c.pushBlob(name, config)
	ld := c.pushBlob(name, []byte(layer))
	manifest = []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"%s","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"%s","size":%d},`+
		`"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":"%s","size":%d}]}`,
		registry.MediaTypeOCIManifest, cd, len(config), ld, len(layer)))
	r := c.do("PUT", "/v2/"+name+"/manifests/"+tag, manifest, "Content-Type", registry.MediaTypeOCIManifest)
	if r.StatusCode != http.StatusCreated {
		c.e.t.Fatalf("put manifest: %d %s", r.StatusCode, r.body)
	}
	return manifest, r.Header.Get("Docker-Content-Digest")
}

func TestAuthentication(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	anon := &client{e: e}
	r := anon.do("GET", "/v2/", nil)
	if r.StatusCode != http.StatusUnauthorized || !strings.Contains(r.Header.Get("WWW-Authenticate"), `Bearer realm="`+e.srv.URL+`/v2/token"`) {
		t.Errorf("anonymous /v2/: %d %v", r.StatusCode, r.Header)
	}
	if r := anon.do("GET", "/v2/token", nil); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous token without public read: %d", r.StatusCode)
	}
	if r := anon.do("POST", "/v2/token", nil); r.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /v2/token: %d", r.StatusCode)
	}
	req, _ := http.NewRequest("GET", e.srv.URL+"/v2/token", nil)
	req.SetBasicAuth("writer", "wrong-password")
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong password: %d", resp.StatusCode)
	}

	w := e.login("writer", testutil.Password("writer"))
	if r := w.do("GET", "/v2/", nil); r.StatusCode != http.StatusOK || r.Header.Get("Docker-Distribution-API-Version") != "registry/2.0" {
		t.Errorf("/v2/ with a token: %d", r.StatusCode)
	}
	w.image("team/app", "1.0", "layer")

	// Readers pull but can't push or delete.
	rd := e.login("reader", testutil.Password("reader"))
	if r := rd.do("GET", "/v2/team/app/manifests/1.0", nil); r.StatusCode != http.StatusOK {
		t.Errorf("reader pull: %d", r.StatusCode)
	}
	if r := rd.do("POST", "/v2/team/app/blobs/uploads/", nil); r.StatusCode != http.StatusForbidden || r.code() != "DENIED" {
		t.Errorf("reader push: %d %s", r.StatusCode, r.body)
	}
	if r := rd.do("DELETE", "/v2/team/app/manifests/1.0", nil); r.StatusCode != http.StatusForbidden {
		t.Errorf("reader delete: %d", r.StatusCode)
	}
	// Anonymous requests are challenged with the scope they need.
	r = anon.do("POST", "/v2/team/app/blobs/uploads/", nil)
	if r.StatusCode != http.StatusUnauthorized || !strings.Contains(r.Header.Get("WWW-Authenticate"), `scope="repository:team/app:pull,push"`) {
		t.Errorf("anonymous push: %d %v", r.StatusCode, r.Header)
	}
	// Access tokens work as Basic password and as Bearer; forged tokens don't.
	pat, _, _ := e.auth.NewToken(ctx, e.writer.ID, "ci")
	if r := (&client{e: e, token: pat}).do("GET", "/v2/_catalog", nil); r.StatusCode != http.StatusOK {
		t.Errorf("bearer PAT: %d", r.StatusCode)
	}
	req, _ = http.NewRequest("GET", e.srv.URL+"/v2/team/app/tags/list", nil)
	req.SetBasicAuth("x", pat)
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != http.StatusOK {
		t.Errorf("basic PAT: %d", resp.StatusCode)
	}
	if r := (&client{e: e, token: "ogr.u1.9999999999.forged"}).do("GET", "/v2/_catalog", nil); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("forged registry token: %d", r.StatusCode)
	}
	// A deactivated user's token stops working at once.
	e.writer.Active = false
	testutil.Must(t, e.st.UpdateUser(ctx, e.writer))
	if r := w.do("GET", "/v2/_catalog", nil); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("token of a deactivated user: %d", r.StatusCode)
	}
}

func TestPublicRead(t *testing.T) {
	t.Parallel()
	e := setup(t, func(c *config.Config) { c.Repo.PublicRead = true })
	e.login("writer", testutil.Password("writer")).image("app", "latest", "l")
	anon := e.login("", "")
	if r := anon.do("GET", "/v2/app/manifests/latest", nil); r.StatusCode != http.StatusOK {
		t.Errorf("anonymous pull with public read: %d", r.StatusCode)
	}
	if r := anon.do("DELETE", "/v2/app/manifests/latest", nil); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous delete: %d", r.StatusCode)
	}
	// /v2/ still challenges anonymous clients so docker login asks for credentials.
	if r := (&client{e: e}).do("GET", "/v2/", nil); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous /v2/: %d", r.StatusCode)
	}
}

func TestPushPull(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	c := e.login("writer", testutil.Password("writer"))
	manifest, digest := c.image("team/app", "1.0", "layer-content")
	if digest != digestOf(manifest) {
		t.Errorf("digest = %s", digest)
	}
	for _, ref := range []string{"1.0", digest} {
		r := c.do("GET", "/v2/team/app/manifests/"+ref, nil)
		if r.StatusCode != http.StatusOK || !bytes.Equal(r.body, manifest) || r.Header.Get("Content-Type") != registry.MediaTypeOCIManifest ||
			r.Header.Get("Docker-Content-Digest") != digest {
			t.Errorf("GET manifest %s: %d %s", ref, r.StatusCode, r.Header)
		}
	}
	if r := c.do("HEAD", "/v2/team/app/manifests/1.0", nil); r.StatusCode != http.StatusOK || len(r.body) != 0 {
		t.Errorf("HEAD manifest: %d", r.StatusCode)
	}
	if r := c.do("GET", "/v2/team/app/manifests/2.0", nil); r.StatusCode != http.StatusNotFound || r.code() != "MANIFEST_UNKNOWN" {
		t.Errorf("unknown tag: %d %s", r.StatusCode, r.body)
	}

	ld := digestOf([]byte("layer-content"))
	r := c.do("GET", "/v2/team/app/blobs/"+ld, nil)
	if r.StatusCode != http.StatusOK || string(r.body) != "layer-content" {
		t.Errorf("GET blob: %d %q", r.StatusCode, r.body)
	}
	r = c.do("GET", "/v2/other/name/blobs/"+ld, nil, "Range", "bytes=6-12")
	if r.StatusCode != http.StatusPartialContent || string(r.body) != "content" {
		t.Errorf("range GET (blobs are global): %d %q", r.StatusCode, r.body)
	}
	if r := c.do("HEAD", "/v2/team/app/blobs/"+ld, nil); r.StatusCode != http.StatusOK || r.Header.Get("Content-Length") != "13" {
		t.Errorf("HEAD blob: %d %v", r.StatusCode, r.Header)
	}
	if r := c.do("GET", "/v2/team/app/blobs/sha256:"+strings.Repeat("0", 64), nil); r.StatusCode != http.StatusNotFound || r.code() != "BLOB_UNKNOWN" {
		t.Errorf("unknown blob: %d", r.StatusCode)
	}
	if r := c.do("GET", "/v2/team/app/blobs/md5:abc", nil); r.StatusCode != http.StatusBadRequest {
		t.Errorf("bad digest: %d", r.StatusCode)
	}
	if r := c.do("DELETE", "/v2/team/app/blobs/"+ld, nil); r.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("DELETE blob: %d", r.StatusCode)
	}

	// Validation.
	for name, tc := range map[string]struct {
		path, body, code string
		status           int
	}{
		"bad name":        {"/v2/Team/App/manifests/x", `{}`, "NAME_INVALID", 400},
		"bad tag":         {"/v2/team/app/manifests/-bad", `{"schemaVersion":2}`, "TAG_INVALID", 400},
		"digest mismatch": {"/v2/team/app/manifests/sha256:" + strings.Repeat("a", 64), `{"schemaVersion":2}`, "DIGEST_INVALID", 400},
		"schema 1":        {"/v2/team/app/manifests/x", `{"schemaVersion":1}`, "MANIFEST_INVALID", 400},
		"no config":       {"/v2/team/app/manifests/x", `{"schemaVersion":2,"mediaType":"` + registry.MediaTypeOCIManifest + `"}`, "MANIFEST_INVALID", 400},
		"unknown type":    {"/v2/team/app/manifests/x", `{"schemaVersion":2,"mediaType":"text/plain"}`, "MANIFEST_INVALID", 415},
		"unknown blob": {"/v2/team/app/manifests/x", `{"schemaVersion":2,"config":{"mediaType":"x","digest":"sha256:` +
			strings.Repeat("b", 64) + `","size":1},"layers":[]}`, "MANIFEST_BLOB_UNKNOWN", 400},
		"bad descriptor": {"/v2/team/app/manifests/x", `{"schemaVersion":2,"config":{"digest":"sha256:xyz"}}`, "MANIFEST_INVALID", 400},
	} {
		r := c.do("PUT", tc.path, []byte(tc.body))
		if r.StatusCode != tc.status || r.code() != tc.code {
			t.Errorf("%s: %d %s", name, r.StatusCode, r.body)
		}
	}
	if r := c.do("GET", "/v2/team/app/unknown", nil); r.StatusCode != http.StatusNotFound {
		t.Errorf("unknown endpoint: %d", r.StatusCode)
	}
	if r := c.do("PATCH", "/v2/team/app/manifests/1.0", nil); r.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("PATCH manifest: %d", r.StatusCode)
	}
}

func TestChunkedUpload(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	c := e.login("writer", testutil.Password("writer"))
	data := bytes.Repeat([]byte("0123456789"), 1000)
	d := digestOf(data)

	r := c.do("POST", "/v2/app/blobs/uploads/", nil)
	loc := r.Header.Get("Location")
	if r.StatusCode != http.StatusAccepted || !strings.HasPrefix(loc, "/v2/app/blobs/uploads/") || r.Header.Get("Range") != "0-0" {
		t.Fatalf("start upload: %d %v", r.StatusCode, r.Header)
	}
	r = c.do("PATCH", loc, data[:4000], "Content-Range", "0-3999")
	if r.StatusCode != http.StatusAccepted || r.Header.Get("Range") != "0-3999" {
		t.Fatalf("first chunk: %d %v %s", r.StatusCode, r.Header, r.body)
	}
	// A chunk that doesn't start at the current offset is refused.
	if r := c.do("PATCH", loc, data[5000:6000], "Content-Range", "5000-5999"); r.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("out-of-order chunk: %d", r.StatusCode)
	}
	if r := c.do("GET", loc, nil); r.StatusCode != http.StatusNoContent || r.Header.Get("Range") != "0-3999" {
		t.Errorf("upload status: %d %v", r.StatusCode, r.Header)
	}
	// The last chunk comes with the final PUT.
	r = c.do("PUT", loc+"?digest="+d, data[4000:])
	if r.StatusCode != http.StatusCreated || r.Header.Get("Docker-Content-Digest") != d {
		t.Fatalf("finish upload: %d %s", r.StatusCode, r.body)
	}
	if r := c.do("GET", "/v2/app/blobs/"+d, nil); !bytes.Equal(r.body, data) {
		t.Errorf("assembled blob differs (%d bytes)", len(r.body))
	}
	if r := c.do("GET", loc, nil); r.StatusCode != http.StatusNotFound || r.code() != "BLOB_UPLOAD_UNKNOWN" {
		t.Errorf("finished upload still exists: %d", r.StatusCode)
	}

	// A digest mismatch is refused, and the upload can be cancelled.
	r = c.do("POST", "/v2/app/blobs/uploads/", nil)
	loc = r.Header.Get("Location")
	if r := c.do("PUT", loc+"?digest="+digestOf([]byte("other")), []byte("content")); r.StatusCode != http.StatusBadRequest || r.code() != "DIGEST_INVALID" {
		t.Errorf("digest mismatch: %d %s", r.StatusCode, r.body)
	}
	r = c.do("POST", "/v2/app/blobs/uploads/", nil)
	loc = r.Header.Get("Location")
	if r := c.do("DELETE", loc, nil); r.StatusCode != http.StatusNoContent {
		t.Errorf("cancel upload: %d", r.StatusCode)
	}
	// An upload belongs to its repository.
	r = c.do("POST", "/v2/app/blobs/uploads/", nil)
	other := strings.Replace(r.Header.Get("Location"), "/v2/app/", "/v2/other/", 1)
	if r := c.do("PATCH", other, []byte("x")); r.StatusCode != http.StatusNotFound {
		t.Errorf("upload used from another repository: %d", r.StatusCode)
	}

	// Pushing a known blob again keeps one copy; an empty blob works.
	c.pushBlob("app", data)
	c.pushBlob("app", []byte{})
	// Cross-repository mount of a known digest.
	r = c.do("POST", "/v2/elsewhere/blobs/uploads/?mount="+d+"&from=app", nil)
	if r.StatusCode != http.StatusCreated || r.Header.Get("Location") != "/v2/elsewhere/blobs/"+d {
		t.Errorf("mount: %d %v", r.StatusCode, r.Header)
	}
	// Mounting an unknown digest starts a normal upload.
	if r := c.do("POST", "/v2/elsewhere/blobs/uploads/?mount=sha256:"+strings.Repeat("c", 64), nil); r.StatusCode != http.StatusAccepted {
		t.Errorf("mount of an unknown blob: %d", r.StatusCode)
	}
}

func TestSizeLimits(t *testing.T) {
	t.Parallel()
	e := setup(t, func(c *config.Config) { c.Registry.MaxBlobBytes, c.Registry.MaxTotalBytes = 100, 150 })
	c := e.login("writer", testutil.Password("writer"))
	big := bytes.Repeat([]byte("x"), 101)
	r := c.do("POST", "/v2/app/blobs/uploads/?digest="+digestOf(big), big)
	if r.StatusCode != http.StatusRequestEntityTooLarge || r.code() != "SIZE_INVALID" {
		t.Errorf("blob over the limit: %d %s", r.StatusCode, r.body)
	}
	c.pushBlob("app", bytes.Repeat([]byte("a"), 100))
	second := bytes.Repeat([]byte("b"), 60)
	if r := c.do("POST", "/v2/app/blobs/uploads/?digest="+digestOf(second), second); r.StatusCode != http.StatusRequestEntityTooLarge || r.code() != "DENIED" {
		t.Errorf("over the total quota: %d %s", r.StatusCode, r.body)
	}
}

func TestIndexesTagsAndDeletes(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	c := e.login("writer", testutil.Password("writer"))
	amd, amdDigest := c.image("multi", "amd", "layer-a")
	_, armDigest := c.image("multi", "arm", "layer-b")
	index := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"%s","manifests":[`+
		`{"mediaType":"%s","digest":"%s","size":%d,"platform":{"os":"linux","architecture":"amd64"}},`+
		`{"mediaType":"%s","digest":"%s","size":10,"platform":{"os":"linux","architecture":"arm64","variant":"v8"}}]}`,
		registry.MediaTypeOCIIndex, registry.MediaTypeOCIManifest, amdDigest, len(amd), registry.MediaTypeOCIManifest, armDigest))
	r := c.do("PUT", "/v2/multi/manifests/latest", index, "Content-Type", registry.MediaTypeOCIIndex)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("put index: %d %s", r.StatusCode, r.body)
	}
	indexDigest := r.Header.Get("Docker-Content-Digest")
	if got := registry.Platforms(index); strings.Join(got, ",") != "linux/amd64,linux/arm64/v8" {
		t.Errorf("Platforms = %v", got)
	}
	missing := strings.Replace(string(index), amdDigest, "sha256:"+strings.Repeat("d", 64), 1)
	if r := c.do("PUT", "/v2/multi/manifests/broken", []byte(missing)); r.code() != "MANIFEST_BLOB_UNKNOWN" {
		t.Errorf("index with an unknown child: %d %s", r.StatusCode, r.body)
	}

	// Tags list with pagination.
	r = c.do("GET", "/v2/multi/tags/list?n=2", nil)
	var tags struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	json.Unmarshal(r.body, &tags)
	if tags.Name != "multi" || strings.Join(tags.Tags, ",") != "amd,arm" || !strings.Contains(r.Header.Get("Link"), "last=arm") {
		t.Errorf("tags page 1: %+v %v", tags, r.Header.Get("Link"))
	}
	r = c.do("GET", "/v2/multi/tags/list?n=2&last=arm", nil)
	json.Unmarshal(r.body, &tags)
	if strings.Join(tags.Tags, ",") != "latest" || r.Header.Get("Link") != "" {
		t.Errorf("tags page 2: %+v", tags)
	}
	if r := c.do("GET", "/v2/nothing/tags/list", nil); r.StatusCode != http.StatusNotFound || r.code() != "NAME_UNKNOWN" {
		t.Errorf("tags of an unknown repository: %d", r.StatusCode)
	}
	r = c.do("GET", "/v2/_catalog", nil)
	if !strings.Contains(string(r.body), `"multi"`) {
		t.Errorf("catalog = %s", r.body)
	}

	// A child of an index can't be deleted while the index exists.
	if r := c.do("DELETE", "/v2/multi/manifests/"+amdDigest, nil); r.StatusCode != http.StatusConflict {
		t.Errorf("delete a referenced child: %d %s", r.StatusCode, r.body)
	}
	// Deleting a tag leaves the manifest as an untagged version.
	if r := c.do("DELETE", "/v2/multi/manifests/amd", nil); r.StatusCode != http.StatusAccepted {
		t.Errorf("delete tag: %d %s", r.StatusCode, r.body)
	}
	if r := c.do("GET", "/v2/multi/manifests/"+amdDigest, nil); r.StatusCode != http.StatusOK {
		t.Errorf("manifest gone with its tag: %d", r.StatusCode)
	}
	// Deleting the index by digest removes it and prunes untagged children.
	if r := c.do("DELETE", "/v2/multi/manifests/"+indexDigest, nil); r.StatusCode != http.StatusAccepted {
		t.Errorf("delete index: %d %s", r.StatusCode, r.body)
	}
	if r := c.do("GET", "/v2/multi/manifests/"+amdDigest, nil); r.StatusCode != http.StatusNotFound {
		t.Errorf("untagged child survived: %d", r.StatusCode)
	}
	if r := c.do("GET", "/v2/multi/manifests/arm", nil); r.StatusCode != http.StatusOK {
		t.Errorf("tagged child pruned: %d", r.StatusCode)
	}
	if r := c.do("DELETE", "/v2/multi/manifests/nope", nil); r.StatusCode != http.StatusNotFound {
		t.Errorf("delete an unknown tag: %d", r.StatusCode)
	}
}

func TestDetailsAndAPI(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	c := e.login("writer", testutil.Password("writer"))
	_, digest := c.image("team/app", "1.0", "layer")
	c.image("team/web", "2.0", "web-layer")
	c.image("solo", "x", "solo-layer")

	m, err := e.st.RegistryManifestByTag(ctx, "team/app", "1.0")
	testutil.Must(t, err)
	if m.Platform != "linux/arm64/v8" || *m.PushedBy != e.writer.ID || m.TotalSize == 0 {
		t.Errorf("manifest = %+v", m)
	}
	d, err := e.svc.Details(ctx, m, "")
	testutil.Must(t, err)
	img := d.Image
	if img == nil || img.Platform != "linux/arm64/v8" || img.Command != "/app" || len(img.Labels) != 1 || len(img.Ports) != 1 ||
		len(img.Layers) != 2 || img.Layers[0].Command != "ADD file:abc in /" || img.Layers[0].Digest == "" || img.Layers[1].Digest != "" {
		t.Errorf("details = %+v", img)
	}

	api := func(method, path, token string) result {
		t.Helper()
		req, _ := http.NewRequest(method, e.srv.URL+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "token "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return result{resp, b}
	}
	pat, _, _ := e.auth.NewToken(ctx, e.writer.ID, "api")
	readerPAT, _, _ := e.auth.NewToken(ctx, e.reader.ID, "api")
	if r := api("GET", "/api/v1/packages/team", ""); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous API: %d", r.StatusCode)
	}
	r := api("GET", "/api/v1/packages/team?type=container&limit=1", pat)
	var list []struct {
		Name, Version, Type string
		HTMLURL             string `json:"html_url"`
	}
	json.Unmarshal(r.body, &list)
	if r.StatusCode != http.StatusOK || r.Header.Get("X-Total-Count") != "2" || len(list) != 1 || list[0].Type != "container" ||
		!strings.Contains(r.Header.Get("Link"), "page=2") {
		t.Errorf("list: %d %s %v", r.StatusCode, r.body, r.Header)
	}
	if r := api("GET", "/api/v1/packages/team?type=npm", pat); string(bytes.TrimSpace(r.body)) != "[]" {
		t.Errorf("other package type: %s", r.body)
	}
	r = api("GET", "/api/v1/packages/team/container/app/1.0", pat)
	var pkg struct {
		Name, Version string
		Creator       struct{ Login string }
		HTMLURL       string `json:"html_url"`
	}
	json.Unmarshal(r.body, &pkg)
	if pkg.Name != "app" || pkg.Version != "1.0" || pkg.Creator.Login != "writer" || pkg.HTMLURL != e.srv.URL+"/packages/team/app/-/1.0" {
		t.Errorf("version: %s", r.body)
	}
	// %2F-encoded names work too.
	if r := api("GET", "/api/v1/packages/team/container/app/"+strings.Replace(digest, ":", "%3A", 1)+"/files", pat); !strings.Contains(string(r.body), "manifest.json") {
		t.Errorf("files: %s", r.body)
	}
	if r := api("GET", "/api/v1/packages/team/container/app/9.9", pat); r.StatusCode != http.StatusNotFound {
		t.Errorf("unknown version: %d", r.StatusCode)
	}
	if r := api("DELETE", "/api/v1/packages/team/container/app/1.0", readerPAT); r.StatusCode != http.StatusForbidden {
		t.Errorf("reader delete: %d", r.StatusCode)
	}
	if r := api("DELETE", "/api/v1/packages/team/container/app/1.0", pat); r.StatusCode != http.StatusNoContent {
		t.Errorf("delete: %d %s", r.StatusCode, r.body)
	}
	if r := api("GET", "/api/v1/packages/team/pypi/app", pat); r.StatusCode != http.StatusNotFound {
		t.Errorf("non-container path: %d", r.StatusCode)
	}
	if got := registry.VersionURL("team/app", "sha256:x"); got != "/packages/team/app/-/sha256:x" {
		t.Errorf("VersionURL = %s", got)
	}
}

func TestCleanupAndGC(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	c := e.login("writer", testutil.Password("writer"))
	for _, tag := range []string{"pr-1", "pr-2", "v1", "latest", "pr-3"} {
		c.image("app", tag, "layer-"+tag)
		time.Sleep(10 * time.Millisecond) // distinct push times
	}
	rule := &store.RegistryCleanupRule{Enabled: true, KeepCount: 1, RemovePattern: `pr-\d+`}
	plan, err := e.svc.CleanupPlan(ctx, rule)
	testutil.Must(t, err)
	var names []string
	for _, p := range plan {
		names = append(names, p.Version)
	}
	// pr-3 is the newest (kept by count), latest is always kept, v1 doesn't match.
	if strings.Join(names, ",") != "pr-2,pr-1" {
		t.Errorf("plan = %v", names)
	}
	rule.RemoveDays = 30
	if plan, _ := e.svc.CleanupPlan(ctx, rule); len(plan) != 0 {
		t.Errorf("young versions planned for removal: %v", plan)
	}
	rule.RemoveDays, rule.KeepPattern, rule.MatchFullName = 0, `app/pr-1`, true
	testutil.Must(t, e.st.SaveRegistryCleanupRule(ctx, rule))
	removed, err := e.svc.RunCleanup(ctx, false)
	if err != nil || removed != 0 {
		// The full-name remove pattern "pr-\d+" can't match "app/pr-2".
		t.Errorf("RunCleanup = %d, %v", removed, err)
	}
	rule.RemovePattern = `app/pr-\d+`
	testutil.Must(t, e.st.SaveRegistryCleanupRule(ctx, rule))
	if removed, err := e.svc.RunCleanup(ctx, false); err != nil || removed != 1 {
		t.Errorf("RunCleanup = %d, %v", removed, err)
	}
	if r := c.do("GET", "/v2/app/manifests/pr-2", nil); r.StatusCode != http.StatusNotFound {
		t.Errorf("pr-2 survived cleanup: %d", r.StatusCode)
	}
	saved, _ := e.st.RegistryCleanupRule(ctx)
	if saved.LastRunAt == nil || saved.LastRemoved != 1 {
		t.Errorf("rule after run = %+v", saved)
	}
	rule.Enabled = false
	testutil.Must(t, e.st.SaveRegistryCleanupRule(ctx, rule))
	if n, _ := e.svc.RunCleanup(ctx, false); n != 0 {
		t.Error("a disabled rule ran")
	}

	// GC frees the layer of the removed version (but not shared config blobs).
	orphan := digestOf([]byte("layer-pr-2"))
	blobRow, err := e.st.RegistryBlob(ctx, orphan)
	testutil.Must(t, err)
	stats, err := e.svc.GC(ctx, time.Hour)
	if err != nil || stats.Blobs != 0 {
		t.Errorf("GC within the grace period: %+v, %v", stats, err)
	}
	time.Sleep(20 * time.Millisecond)
	c.do("POST", "/v2/app/blobs/uploads/", nil) // a stale upload
	stats, err = e.svc.GC(ctx, 10*time.Millisecond)
	if err != nil || stats.Blobs != 1 || stats.BytesFreed != int64(len("layer-pr-2")) {
		t.Errorf("GC = %+v, %v", stats, err)
	}
	if _, err := e.st.RegistryBlob(ctx, orphan); err == nil {
		t.Error("orphan blob row survived GC")
	}
	if ok, _ := e.blobs.Exists(ctx, blobRow.S3Key); ok {
		t.Error("orphan blob object survived GC")
	}
	if r := c.do("GET", "/v2/app/manifests/v1", nil); r.StatusCode != http.StatusOK {
		t.Error("GC broke a live image")
	}
	if r := c.do("GET", "/v2/app/blobs/"+digestOf([]byte("layer-v1")), nil); r.StatusCode != http.StatusOK {
		t.Error("GC removed a referenced blob")
	}
}

func TestCIProvenance(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	sha := strings.Repeat("c", 40)
	run := &store.Run{Kind: "pipeline", Name: "ci", File: "ci.yml", Event: "push", Ref: "refs/heads/main", SHA: sha,
		TriggeredBy: &e.reader.ID, Status: store.JobQueued}
	job := &store.Job{Name: "build", Kind: "ci", Spec: []byte("{}"), Status: store.JobQueued}
	testutil.Must(t, e.st.CreateRun(ctx, run, []*store.Job{job}))
	runner := &store.Runner{Name: "r", Kind: "ci", Labels: []string{}}
	testutil.Must(t, e.st.CreateRunner(ctx, runner, "h"))
	jobToken := auth.JobTokenPrefix + auth.RandomString(20)
	claimed, err := e.st.ClaimJob(ctx, runner, auth.HashToken(jobToken), func(*store.Job) bool { return true })
	if err != nil || claimed == nil {
		t.Fatal(err)
	}
	// The job logs in with its token and pushes (even though the user who
	// triggered it is only a reader).
	ci := e.login("ci", jobToken)
	_, digest := ci.image("app", sha, "built-by-ci")
	m, _ := e.st.RegistryManifestByTag(ctx, "app", sha)
	if m.BuildSHA != sha || m.BuildRef != "refs/heads/main" || *m.BuildJobID != job.ID || *m.PushedBy != e.reader.ID {
		t.Errorf("provenance = %+v", m)
	}
	// Jobs can't delete.
	if r := ci.do("DELETE", "/v2/app/manifests/"+digest, nil); r.StatusCode != http.StatusForbidden {
		t.Errorf("job delete: %d", r.StatusCode)
	}
	// People can't move a CI-built tag, but may push other tags.
	w := e.login("writer", testutil.Password("writer"))
	manifest, _ := w.image("app", "hand", "by-hand")
	if r := w.do("PUT", "/v2/app/manifests/"+sha, manifest); r.StatusCode != http.StatusForbidden {
		t.Errorf("overwrite a CI tag: %d %s", r.StatusCode, r.body)
	}
	// Once the job ends its registry token is dead.
	e.st.FinishJob(ctx, job.ID, store.JobSuccess, "")
	if r := ci.do("GET", "/v2/app/manifests/"+sha, nil); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("token of a finished job: %d", r.StatusCode)
	}
}
