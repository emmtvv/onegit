package registry

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"onegit/internal/auth"
	"onegit/internal/store"
)

// Gitea-compatible packages API (container type only), so scripts written
// against Gitea's /api/v1/packages keep working. A Gitea image
// host/<owner>/<name> is the onegit image "<owner>/<name>".
//
//	GET    /api/v1/packages/{owner}?type=container&q=&page=&limit=
//	GET    /api/v1/packages/{owner}/container/{name}
//	GET    /api/v1/packages/{owner}/container/{name}/{version}
//	DELETE /api/v1/packages/{owner}/container/{name}/{version}
//	GET    /api/v1/packages/{owner}/container/{name}/{version}/files
const apiPrefix = "/api/v1/packages/"

// MatchAPI reports whether the request is for the packages API.
func MatchAPI(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, apiPrefix) }

type apiUser struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	LoginName string `json:"login_name"`
	FullName  string `json:"full_name"`
	Email     string `json:"email"`
	AvatarURL string `json:"avatar_url"`
	Username  string `json:"username"`
}

type apiPackage struct {
	ID         int64     `json:"id"`
	Owner      apiUser   `json:"owner"`
	Repository any       `json:"repository"`
	Creator    apiUser   `json:"creator"`
	Type       string    `json:"type"`
	Name       string    `json:"name"`
	Version    string    `json:"version"`
	HTMLURL    string    `json:"html_url"`
	CreatedAt  time.Time `json:"created_at"`
}

type apiFile struct {
	ID     int64  `json:"id"`
	Size   int64  `json:"Size"`
	Name   string `json:"name"`
	MD5    string `json:"md5"`
	SHA1   string `json:"sha1"`
	SHA256 string `json:"sha256"`
	SHA512 string `json:"sha512"`
}

func giteaError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"message": msg, "url": "https://docs.gitea.com/api"})
}

// apiUserFor authenticates like Gitea: "Authorization: token|Bearer <PAT>",
// Basic auth (password or token), or ?token= / ?access_token=.
func (s *Service) apiUserFor(r *http.Request) (*store.User, error) {
	h := r.Header.Get("Authorization")
	tok := r.URL.Query().Get("token")
	if tok == "" {
		tok = r.URL.Query().Get("access_token")
	}
	if t, ok := strings.CutPrefix(h, "token "); ok {
		tok = t
	} else if t, ok := strings.CutPrefix(h, "Bearer "); ok {
		tok = t
	}
	switch {
	case tok != "":
		return s.Auth.CheckToken(r.Context(), tok)
	case strings.HasPrefix(h, "Basic "):
		user, pass, _ := r.BasicAuth()
		return s.Auth.CheckBasic(r.Context(), user, pass, auth.RequestIP(r, s.Cfg.HTTP.TrustProxy))
	}
	return nil, errNoCredentials
}

func (s *Service) ServeAPI(w http.ResponseWriter, r *http.Request) {
	u, err := s.apiUserFor(r)
	switch {
	case err != nil && !errors.Is(err, errNoCredentials):
		giteaError(w, http.StatusUnauthorized, err.Error())
		return
	case u == nil && !s.Cfg.Repo.PublicRead:
		giteaError(w, http.StatusUnauthorized, "authentication required")
		return
	}

	var parts []string
	for _, p := range strings.Split(strings.Trim(strings.TrimPrefix(r.URL.EscapedPath(), apiPrefix), "/"), "/") {
		p, err := url.PathUnescape(p)
		if err != nil {
			giteaError(w, http.StatusBadRequest, "bad path")
			return
		}
		parts = append(parts, p)
	}
	owner := parts[0]
	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			giteaError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if t := r.URL.Query().Get("type"); t != "" && t != "container" {
			writeJSON(w, []apiPackage{})
			return
		}
		s.apiList(w, r, store.RegistryVersionFilter{RepoPrefix: owner + "/", RepoQuery: r.URL.Query().Get("q")}, owner)
		return
	}
	if len(parts) < 3 || parts[1] != "container" {
		giteaError(w, http.StatusNotFound, "package not found")
		return
	}
	repo := owner + "/" + parts[2]
	switch {
	case len(parts) == 3 && r.Method == http.MethodGet:
		s.apiList(w, r, store.RegistryVersionFilter{Repo: repo}, owner)
	case len(parts) == 4 && r.Method == http.MethodGet:
		if v := s.apiVersion(w, r, repo, parts[3]); v != nil {
			writeJSON(w, s.apiPackage(owner, v))
		}
	case len(parts) == 4 && r.Method == http.MethodDelete:
		if u == nil {
			giteaError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		if !u.CanWrite() {
			giteaError(w, http.StatusForbidden, "you do not have write access to packages")
			return
		}
		err := s.DeleteVersion(r.Context(), repo, parts[3])
		switch {
		case errors.Is(err, store.ErrNotFound):
			giteaError(w, http.StatusNotFound, "package version not found")
		case errors.Is(err, store.ErrReferenced):
			giteaError(w, http.StatusConflict, "manifest is referenced by an index")
		case err != nil:
			s.internalError(w, r, err)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	case len(parts) == 5 && parts[4] == "files" && r.Method == http.MethodGet:
		if v := s.apiVersion(w, r, repo, parts[3]); v != nil {
			writeJSON(w, apiFiles(v))
		}
	default:
		giteaError(w, http.StatusNotFound, "not found")
	}
}

// apiVersion finds a version by tag or digest; on failure the response is
// written.
func (s *Service) apiVersion(w http.ResponseWriter, r *http.Request, repo, version string) *store.RegistryVersion {
	m, err := s.lookupManifest(r, repo, version)
	if errors.Is(err, store.ErrNotFound) {
		giteaError(w, http.StatusNotFound, "package version not found")
		return nil
	}
	if err != nil {
		s.internalError(w, r, err)
		return nil
	}
	v := &store.RegistryVersion{Name: version, Tagged: !validDigest(version), UpdatedAt: m.CreatedAt, Manifest: *m}
	if m.PushedBy != nil {
		if pu, err := s.Store.UserByID(r.Context(), *m.PushedBy); err == nil {
			v.PushedBy = pu.Username
		}
	}
	return v
}

func (s *Service) apiList(w http.ResponseWriter, r *http.Request, f store.RegistryVersionFilter, owner string) {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	page = max(page, 1)
	if limit <= 0 || limit > 50 {
		limit = 30
	}
	f.Limit, f.Offset = limit, (page-1)*limit
	versions, total, err := s.Store.ListRegistryVersions(r.Context(), f)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]apiPackage, 0, len(versions))
	for i := range versions {
		out = append(out, s.apiPackage(owner, &versions[i]))
	}
	w.Header().Set("X-Total-Count", strconv.Itoa(total))
	if page*limit < total {
		next := *r.URL
		nq := next.Query()
		nq.Set("page", strconv.Itoa(page+1))
		nq.Set("limit", strconv.Itoa(limit))
		next.RawQuery = nq.Encode()
		w.Header().Set("Link", `<`+s.Cfg.HTTP.BaseURL+next.RequestURI()+`>; rel="next"`)
	}
	writeJSON(w, out)
}

func (s *Service) apiPackage(owner string, v *store.RegistryVersion) apiPackage {
	name := strings.TrimPrefix(v.Manifest.Repo, owner+"/")
	return apiPackage{
		ID:        v.Manifest.ID,
		Owner:     apiUser{Login: owner, Username: owner},
		Creator:   apiUser{Login: v.PushedBy, Username: v.PushedBy},
		Type:      "container",
		Name:      name,
		Version:   v.Name,
		HTMLURL:   s.Cfg.HTTP.BaseURL + VersionURL(v.Manifest.Repo, v.Name),
		CreatedAt: v.UpdatedAt,
	}
}

// apiFiles lists a version's "files" the way Gitea does for containers: the
// manifest plus the blobs it references.
func apiFiles(v *store.RegistryVersion) []apiFile {
	m := v.Manifest
	files := []apiFile{{Name: "manifest.json", Size: int64(len(m.Content)), SHA256: strings.TrimPrefix(m.Digest, "sha256:")}}
	var doc Manifest
	if err := json.Unmarshal(m.Content, &doc); err == nil && doc.Config != nil {
		for _, d := range append([]Descriptor{*doc.Config}, doc.Layers...) {
			files = append(files, apiFile{Name: d.Digest, Size: d.Size, SHA256: strings.TrimPrefix(d.Digest, "sha256:")})
		}
	}
	for i := range files {
		if _, err := hex.DecodeString(files[i].SHA256); err != nil {
			files[i].SHA256 = ""
		}
	}
	return files
}

// VersionURL is the web page of an image version.
func VersionURL(repo, version string) string {
	return "/packages/" + (&url.URL{Path: repo}).EscapedPath() + "/-/" + url.PathEscape(version)
}
