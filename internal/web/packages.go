package web

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"onegit/internal/registry"
	"onegit/internal/store"
)

const (
	versionsPerPage = 30
	imagesPerPage   = 50
)

type packageVersion struct {
	store.RegistryVersion
	Kind      string
	Platforms []string
	URL       string
}

func describeVersion(v store.RegistryVersion) packageVersion {
	pv := packageVersion{RegistryVersion: v, Kind: registry.ShortMediaType(v.Manifest.MediaType),
		URL: registry.VersionURL(v.Manifest.Repo, v.Name)}
	if registry.IsIndex(v.Manifest.MediaType) {
		pv.Platforms = registry.Platforms(v.Manifest.Content)
		if len(pv.Platforms) <= 1 {
			pv.Kind = "image" // e.g. buildx wraps one platform + attestations in an index
		}
	} else if v.Manifest.Platform != "" {
		pv.Platforms = []string{v.Manifest.Platform}
	}
	return pv
}

func (w *Web) packageList(rw http.ResponseWriter, r *http.Request) {
	if w.Registry == nil {
		w.notFound(rw, r)
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)
	images, total, err := w.Store.ListRegistryImages(r.Context(), q, imagesPerPage, (page-1)*imagesPerPage)
	if err != nil {
		w.serverError(rw, r, err)
		return
	}
	w.render(rw, r, http.StatusOK, "packages", &Page{Title: "Packages", Tab: "packages", Data: map[string]any{
		"Images":  images,
		"Query":   q,
		"Host":    w.Cfg.RegistryHost(),
		"Page":    page,
		"HasNext": page*imagesPerPage < total,
	}})
}

// packageView serves /packages/<name> (versions) and
// /packages/<name>/-/<version> (one version).
func (w *Web) packageView(rw http.ResponseWriter, r *http.Request) {
	rest := r.PathValue("rest")
	if w.Registry == nil || rest == "" {
		w.notFound(rw, r)
		return
	}
	if name, version, ok := strings.Cut(rest, "/-/"); ok {
		w.packageVersion(rw, r, name, version)
		return
	}
	name := strings.TrimSuffix(rest, "/")
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)
	versions, total, err := w.Store.ListRegistryVersions(r.Context(), store.RegistryVersionFilter{
		Repo: name, Query: q, Limit: versionsPerPage, Offset: (page - 1) * versionsPerPage,
	})
	if err != nil {
		w.serverError(rw, r, err)
		return
	}
	if total == 0 {
		if ok, _ := w.Store.RegistryRepoExists(r.Context(), name); !ok {
			w.notFound(rw, r)
			return
		}
	}
	view := make([]packageVersion, len(versions))
	latest := ""
	for i, v := range versions {
		view[i] = describeVersion(v)
		if v.Tagged && latest == "" {
			latest = v.Name
		}
	}
	if _, err := w.Store.RegistryManifestByTag(r.Context(), name, "latest"); err == nil {
		latest = "latest"
	}
	w.render(rw, r, http.StatusOK, "package", &Page{Title: name + " · Packages", Tab: "packages", Data: map[string]any{
		"Name":     name,
		"URL":      packageURL(name),
		"Image":    w.Cfg.RegistryHost() + "/" + name,
		"Versions": view,
		"Latest":   latest,
		"Query":    q,
		"Total":    total,
		"Page":     page,
		"HasNext":  page*versionsPerPage < total,
	}})
}

func (w *Web) packageVersion(rw http.ResponseWriter, r *http.Request, name, version string) {
	ctx := r.Context()
	var m *store.RegistryManifest
	var err error
	tagged := !strings.HasPrefix(version, "sha256:")
	if tagged {
		m, err = w.Store.RegistryManifestByTag(ctx, name, version)
	} else {
		m, err = w.Store.RegistryManifestByDigest(ctx, name, version)
	}
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	details, err := w.Registry.Details(ctx, m, r.URL.Query().Get("platform"))
	if err != nil {
		w.serverError(rw, r, err)
		return
	}
	tags, err := w.Store.RegistryTagsFor(ctx, m.ID)
	if err != nil {
		w.serverError(rw, r, err)
		return
	}
	pushedBy := "ghost"
	if m.PushedBy != nil {
		if u, err := w.Store.UserByID(ctx, *m.PushedBy); err == nil {
			pushedBy = u.Username
		}
	}
	image := w.Cfg.RegistryHost() + "/" + name
	title := name + ":" + version
	pull := image + ":" + version
	if !tagged {
		title = name + "@" + version[:min(len(version), 19)]
		pull = image + "@" + m.Digest
	}
	w.render(rw, r, http.StatusOK, "package_version", &Page{Title: title + " · Packages", Tab: "packages", Data: map[string]any{
		"Name":         name,
		"URL":          packageURL(name),
		"Version":      version,
		"Tagged":       tagged,
		"Heading":      title,
		"Manifest":     m,
		"Kind":         describeVersion(store.RegistryVersion{Manifest: *m}).Kind,
		"Details":      details,
		"Tags":         tags,
		"PushedBy":     pushedBy,
		"Pull":         pull,
		"PullByDigest": image + "@" + m.Digest,
		"Self":         registry.VersionURL(name, version),
	}})
}

func (w *Web) requireWriter(rw http.ResponseWriter, r *http.Request) bool {
	if w.Registry == nil || !currentUser(r).CanWrite() {
		w.errorPage(rw, r, http.StatusForbidden, "You need write access to delete packages.")
		return false
	}
	return true
}

// packageDeleteVersion deletes a tag (and the version behind it when no other
// tag uses it) or an untagged version. Layers are freed by the registry GC.
func (w *Web) packageDeleteVersion(rw http.ResponseWriter, r *http.Request) {
	if !w.requireWriter(rw, r) {
		return
	}
	name, version := r.FormValue("name"), r.FormValue("version")
	back := packageURL(name)
	err := w.Registry.DeleteVersion(r.Context(), name, version)
	switch {
	case errors.Is(err, store.ErrNotFound):
		w.redirectFlash(rw, r, back, "Version "+version+" no longer exists.")
		return
	case errors.Is(err, store.ErrReferenced):
		w.redirectFlash(rw, r, back, "This version is part of a multi-platform image; delete that image instead.")
		return
	case err != nil:
		w.serverError(rw, r, err)
		return
	}
	if ok, _ := w.Store.RegistryRepoExists(r.Context(), name); !ok {
		back = "/packages"
	}
	w.redirectFlash(rw, r, back, "Deleted "+name+" "+version+".")
}

func (w *Web) packageDeleteImage(rw http.ResponseWriter, r *http.Request) {
	if !w.requireWriter(rw, r) {
		return
	}
	name := r.FormValue("name")
	if err := w.Store.DeleteRegistryRepo(r.Context(), name); err != nil && !errors.Is(err, store.ErrNotFound) {
		w.serverError(rw, r, err)
		return
	}
	w.redirectFlash(rw, r, "/packages", "Deleted image "+name+" with all its versions.")
}

func packageURL(name string) string {
	return "/packages/" + (&url.URL{Path: name}).EscapedPath()
}

// ---- admin: cleanup rule ----

func (w *Web) adminPackages(rw http.ResponseWriter, r *http.Request) {
	if w.Registry == nil {
		w.notFound(rw, r)
		return
	}
	rule, err := w.Store.RegistryCleanupRule(r.Context())
	if err != nil {
		w.serverError(rw, r, err)
		return
	}
	w.renderAdminPackages(rw, r, rule, "")
}

func (w *Web) renderAdminPackages(rw http.ResponseWriter, r *http.Request, rule *store.RegistryCleanupRule, formErr string) {
	ctx := r.Context()
	data := map[string]any{"Rule": rule, "MaxBlob": w.Cfg.Registry.MaxBlobBytes, "MaxTotal": w.Cfg.Registry.MaxTotalBytes}
	if formErr == "" {
		plan, err := w.Registry.CleanupPlan(ctx, rule)
		if err != nil {
			w.serverError(rw, r, err)
			return
		}
		var size int64
		for _, c := range plan {
			size += c.Size
		}
		data["Plan"], data["PlanSize"] = plan, size
	}
	used, err := w.Store.RegistryTotalSize(ctx)
	if err != nil {
		w.serverError(rw, r, err)
		return
	}
	data["Used"] = used
	status := http.StatusOK
	if formErr != "" {
		status = http.StatusUnprocessableEntity
	}
	w.render(rw, r, status, "admin_packages", &Page{Title: "Packages · Admin", Tab: "admin", Error: formErr, Data: data})
}

func (w *Web) adminSavePackages(rw http.ResponseWriter, r *http.Request) {
	if w.Registry == nil {
		w.notFound(rw, r)
		return
	}
	keep, _ := strconv.Atoi(r.FormValue("keep_count"))
	days, _ := strconv.Atoi(r.FormValue("remove_days"))
	rule := &store.RegistryCleanupRule{
		Enabled:       r.FormValue("enabled") == "on",
		KeepCount:     keep,
		KeepPattern:   strings.TrimSpace(r.FormValue("keep_pattern")),
		RemoveDays:    days,
		RemovePattern: strings.TrimSpace(r.FormValue("remove_pattern")),
		MatchFullName: r.FormValue("match_full_name") == "on",
	}
	if err := registry.ValidateCleanupRule(rule); err != nil {
		w.renderAdminPackages(rw, r, rule, err.Error())
		return
	}
	if err := w.Store.SaveRegistryCleanupRule(r.Context(), rule); err != nil {
		w.serverError(rw, r, err)
		return
	}
	w.redirectFlash(rw, r, "/admin/packages", "Cleanup rule saved.")
}

func (w *Web) adminRunCleanup(rw http.ResponseWriter, r *http.Request) {
	if w.Registry == nil {
		w.notFound(rw, r)
		return
	}
	n, err := w.Registry.RunCleanup(r.Context(), true)
	if err != nil {
		w.serverError(rw, r, err)
		return
	}
	w.redirectFlash(rw, r, "/admin/packages", "Cleanup removed "+strconv.Itoa(n)+" versions. Their layers are freed by the next garbage collection.")
}
