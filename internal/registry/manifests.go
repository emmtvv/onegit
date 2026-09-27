package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"onegit/internal/store"
)

const maxManifestSize = 4 << 20

const (
	MediaTypeOCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeOCIIndex       = "application/vnd.oci.image.index.v1+json"
	MediaTypeDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
	MediaTypeDockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"

	mediaTypeOCIConfig    = "application/vnd.oci.image.config.v1+json"
	mediaTypeDockerConfig = "application/vnd.docker.container.image.v1+json"
)

func IsIndex(mediaType string) bool {
	return mediaType == MediaTypeOCIIndex || mediaType == MediaTypeDockerList
}

type Platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

func (p *Platform) String() string {
	if p == nil || p.OS == "" || p.OS == "unknown" {
		return ""
	}
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

type Descriptor struct {
	MediaType string    `json:"mediaType"`
	Digest    string    `json:"digest"`
	Size      int64     `json:"size"`
	URLs      []string  `json:"urls,omitempty"`
	Platform  *Platform `json:"platform,omitempty"`
}

// Manifest covers image manifests and indexes, OCI and Docker v2 alike.
type Manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	ArtifactType  string       `json:"artifactType"`
	Config        *Descriptor  `json:"config"`
	Layers        []Descriptor `json:"layers"`
	Manifests     []Descriptor `json:"manifests"`
}

// Platforms lists the platforms of an index's image children (attestations
// and other unknown/unknown entries are skipped).
func Platforms(content []byte) []string {
	var m Manifest
	if json.Unmarshal(content, &m) != nil {
		return nil
	}
	var out []string
	for _, d := range m.Manifests {
		if p := d.Platform.String(); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *Service) lookupManifest(r *http.Request, name, ref string) (*store.RegistryManifest, error) {
	if validDigest(ref) {
		return s.Store.RegistryManifestByDigest(r.Context(), name, ref)
	}
	return s.Store.RegistryManifestByTag(r.Context(), name, ref)
}

func (s *Service) getManifest(w http.ResponseWriter, r *http.Request, name, ref string) {
	m, err := s.lookupManifest(r, name, ref)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown", map[string]string{"name": name, "reference": ref})
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", m.MediaType)
	h.Set("Docker-Content-Digest", m.Digest)
	h.Set("ETag", `"`+m.Digest+`"`)
	h.Set("Content-Length", strconv.Itoa(len(m.Content)))
	if r.Method != http.MethodHead {
		w.Write(m.Content)
	}
}

func (s *Service) putManifest(w http.ResponseWriter, r *http.Request, u *store.User, name, ref string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxManifestSize+1))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if len(body) > maxManifestSize {
		writeError(w, http.StatusRequestEntityTooLarge, "SIZE_INVALID", "manifest is larger than 4 MiB", nil)
		return
	}
	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	var tag string
	switch {
	case validDigest(ref):
		if ref != digest {
			writeError(w, http.StatusBadRequest, "DIGEST_INVALID", "manifest does not match digest",
				map[string]string{"digest": ref, "actual": digest})
			return
		}
	case tagRe.MatchString(ref):
		tag = ref
	default:
		writeError(w, http.StatusBadRequest, "TAG_INVALID", "invalid tag", map[string]string{"tag": ref})
		return
	}

	var doc Manifest
	if err := json.Unmarshal(body, &doc); err != nil || doc.SchemaVersion != 2 {
		writeError(w, http.StatusBadRequest, "MANIFEST_INVALID", "manifest is not a schema 2 image manifest or index", nil)
		return
	}
	mediaType := doc.MediaType
	if mediaType == "" {
		mediaType, _, _ = mime.ParseMediaType(r.Header.Get("Content-Type"))
	}
	if mediaType == "" {
		// OCI allows omitting mediaType; infer from the shape.
		mediaType = MediaTypeOCIManifest
		if doc.Manifests != nil {
			mediaType = MediaTypeOCIIndex
		}
	}

	m := &store.RegistryManifest{Repo: name, Digest: digest, MediaType: mediaType, Content: body, PushedBy: userRef(u)}
	if j := u.Job; j != nil {
		// Pushed by a CI job: record where the image was built from.
		m.BuildJobID, m.BuildSHA, m.BuildRef = &j.JobID, j.SHA, j.Ref
	}
	var blobs, children []string
	var descs []Descriptor
	switch mediaType {
	case MediaTypeOCIManifest, MediaTypeDockerManifest:
		if doc.Config == nil {
			writeError(w, http.StatusBadRequest, "MANIFEST_INVALID", "image manifest has no config", nil)
			return
		}
		descs = append([]Descriptor{*doc.Config}, doc.Layers...)
		for _, d := range descs {
			if len(d.URLs) > 0 {
				continue // foreign layer, fetched from its URLs
			}
			blobs = append(blobs, d.Digest)
			m.TotalSize += d.Size
		}
	case MediaTypeOCIIndex, MediaTypeDockerList:
		descs = doc.Manifests
		for _, d := range descs {
			children = append(children, d.Digest)
		}
	default:
		writeError(w, http.StatusUnsupportedMediaType, "MANIFEST_INVALID", "unsupported manifest media type",
			map[string]string{"mediaType": mediaType})
		return
	}
	for _, d := range descs {
		if !validDigest(d.Digest) {
			writeError(w, http.StatusBadRequest, "MANIFEST_INVALID", "invalid descriptor digest", map[string]string{"digest": d.Digest})
			return
		}
	}
	if c := doc.Config; c != nil && (c.MediaType == mediaTypeOCIConfig || c.MediaType == mediaTypeDockerConfig) {
		m.Platform = s.configPlatform(r, c)
	}

	if tag != "" && u.Job == nil {
		// A tag produced by CI is the provenance deploy rules rely on: people
		// cannot move it (CI can, and admins can delete it first).
		if cur, err := s.Store.RegistryManifestByTag(r.Context(), name, tag); err == nil && cur.BuildSHA != "" && cur.Digest != digest {
			writeError(w, http.StatusForbidden, "DENIED", "tag "+tag+" was built by CI and cannot be overwritten by hand; push another tag", nil)
			return
		}
	}
	err = s.Store.PutRegistryManifest(r.Context(), m, blobs, children, tag)
	var missing *store.MissingRefsError
	if errors.As(err, &missing) {
		writeError(w, http.StatusBadRequest, "MANIFEST_BLOB_UNKNOWN", "manifest references unknown blobs or manifests",
			map[string][]string{"digests": missing.Digests})
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	h := w.Header()
	h.Set("Location", "/v2/"+name+"/manifests/"+digest)
	h.Set("Docker-Content-Digest", digest)
	h.Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
}

// configPlatform reads os/architecture from a (small) image config blob for
// display. Failures just leave the platform empty.
func (s *Service) configPlatform(r *http.Request, c *Descriptor) string {
	if c.Size > 1<<20 {
		return ""
	}
	b, err := s.Store.RegistryBlob(r.Context(), c.Digest)
	if err != nil {
		return ""
	}
	rc, _, err := s.Blob.Get(r.Context(), b.S3Key)
	if err != nil {
		return ""
	}
	defer rc.Close()
	var p Platform
	if json.NewDecoder(io.LimitReader(rc, 1<<20)).Decode(&p) != nil {
		return ""
	}
	return p.String()
}

// deleteManifest deletes a manifest (by digest, with its tags) or just a
// tag (by name), as in OCI distribution 1.1.
func (s *Service) deleteManifest(w http.ResponseWriter, r *http.Request, name, ref string) {
	var err error
	if validDigest(ref) {
		err = s.Store.DeleteRegistryManifest(r.Context(), name, ref)
	} else {
		err = s.Store.DeleteRegistryTag(r.Context(), name, ref, false)
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown", map[string]string{"name": name, "reference": ref})
	case errors.Is(err, store.ErrReferenced):
		writeError(w, http.StatusConflict, "DENIED", "manifest is referenced by an index; delete the index first", nil)
	case err != nil:
		s.internalError(w, r, err)
	default:
		w.WriteHeader(http.StatusAccepted)
	}
}

// pageParams reads ?n= and ?last=; n <= 0 means "no limit" (capped).
func pageParams(r *http.Request) (n int, last string) {
	n, _ = strconv.Atoi(r.URL.Query().Get("n"))
	if n <= 0 || n > 10000 {
		n = 10000
	}
	return n, r.URL.Query().Get("last")
}

func setNextLink(w http.ResponseWriter, r *http.Request, n int, items []string) []string {
	if len(items) <= n {
		return items
	}
	items = items[:n]
	q := url.Values{"n": {strconv.Itoa(n)}, "last": {items[n-1]}}
	w.Header().Set("Link", `<`+r.URL.Path+"?"+q.Encode()+`>; rel="next"`)
	return items
}

func (s *Service) listTags(w http.ResponseWriter, r *http.Request, name string) {
	ok, err := s.Store.RegistryRepoExists(r.Context(), name)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "NAME_UNKNOWN", "repository name not known to registry", map[string]string{"name": name})
		return
	}
	n, last := pageParams(r)
	tags, err := s.Store.RegistryTagNames(r.Context(), name, last, n+1)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"name": name, "tags": nonNil(setNextLink(w, r, n, tags))})
}

func (s *Service) catalog(w http.ResponseWriter, r *http.Request) {
	n, last := pageParams(r)
	repos, err := s.Store.RegistryCatalog(r.Context(), last, n+1)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"repositories": nonNil(setNextLink(w, r, n, repos))})
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ShortMediaType is a human label for the UI.
func ShortMediaType(mt string) string {
	switch {
	case IsIndex(mt):
		return "multi-platform"
	case mt == MediaTypeOCIManifest, mt == MediaTypeDockerManifest:
		return "image"
	}
	return strings.TrimPrefix(mt, "application/")
}
