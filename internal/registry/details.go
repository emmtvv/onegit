package registry

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"time"

	"onegit/internal/store"
)

// ImageConfig is the part of an image config blob shown in the UI.
type ImageConfig struct {
	Created      *time.Time `json:"created"`
	Author       string     `json:"author"`
	OS           string     `json:"os"`
	Architecture string     `json:"architecture"`
	Variant      string     `json:"variant"`
	Config       struct {
		User         string              `json:"User"`
		Env          []string            `json:"Env"`
		Entrypoint   []string            `json:"Entrypoint"`
		Cmd          []string            `json:"Cmd"`
		WorkingDir   string              `json:"WorkingDir"`
		ExposedPorts map[string]struct{} `json:"ExposedPorts"`
		Labels       map[string]string   `json:"Labels"`
	} `json:"config"`
	History []struct {
		Created    *time.Time `json:"created"`
		CreatedBy  string     `json:"created_by"`
		Comment    string     `json:"comment"`
		EmptyLayer bool       `json:"empty_layer"`
	} `json:"history"`
}

// PlatformEntry is one image of a multi-platform index.
type PlatformEntry struct {
	Platform string
	Digest   string
	Size     int64
}

// Layer is one row of the "image layers" table: a history step, with the
// layer it produced (Digest empty for metadata-only steps).
type Layer struct {
	Command string
	Digest  string
	Size    int64
}

type Label struct{ Key, Value string }

// ImageDetails describes one image manifest (one platform).
type ImageDetails struct {
	Digest    string
	Platform  string
	Size      int64
	Created   *time.Time
	Author    string
	Env       []string
	Command   string
	WorkDir   string
	User      string
	Ports     []string
	Labels    []Label
	Layers    []Layer
	MediaType string // config media type for non-image artifacts
}

// VersionDetails is what the version page shows.
type VersionDetails struct {
	Platforms []PlatformEntry // index only
	Image     *ImageDetails   // selected platform (nil for an index without images)
}

// Details resolves a version: for an index, its platforms and the image of
// the selected platform digest (default: the first one).
func (s *Service) Details(ctx context.Context, m *store.RegistryManifest, platformDigest string) (*VersionDetails, error) {
	var d VersionDetails
	image := m
	if IsIndex(m.MediaType) {
		var idx Manifest
		if err := json.Unmarshal(m.Content, &idx); err != nil {
			return nil, err
		}
		image = nil
		for _, c := range idx.Manifests {
			p := c.Platform.String()
			if p == "" {
				continue // attestations
			}
			child, err := s.Store.RegistryManifestByDigest(ctx, m.Repo, c.Digest)
			if err != nil {
				continue
			}
			d.Platforms = append(d.Platforms, PlatformEntry{Platform: p, Digest: c.Digest, Size: child.TotalSize})
			if image == nil || c.Digest == platformDigest {
				image = child
			}
		}
		if image == nil {
			return &d, nil
		}
	}
	img, err := s.imageDetails(ctx, image)
	if err != nil {
		return nil, err
	}
	d.Image = img
	return &d, nil
}

func (s *Service) imageDetails(ctx context.Context, m *store.RegistryManifest) (*ImageDetails, error) {
	var doc Manifest
	if err := json.Unmarshal(m.Content, &doc); err != nil {
		return nil, err
	}
	img := &ImageDetails{Digest: m.Digest, Platform: m.Platform, Size: m.TotalSize}
	if doc.Config == nil {
		return img, nil
	}
	if c := doc.Config.MediaType; c != mediaTypeOCIConfig && c != mediaTypeDockerConfig {
		img.MediaType = c
		for _, l := range doc.Layers {
			img.Layers = append(img.Layers, Layer{Command: l.MediaType, Digest: l.Digest, Size: l.Size})
		}
		return img, nil
	}
	cfg, err := s.imageConfig(ctx, doc.Config)
	if err != nil {
		s.Log.Warn("registry: read image config", "digest", doc.Config.Digest, "err", err)
		cfg = &ImageConfig{}
	}
	img.Created, img.Author = cfg.Created, cfg.Author
	img.Env, img.WorkDir, img.User = cfg.Config.Env, cfg.Config.WorkingDir, cfg.Config.User
	img.Command = strings.TrimSpace(strings.Join(append(append([]string{}, cfg.Config.Entrypoint...), cfg.Config.Cmd...), " "))
	for p := range cfg.Config.ExposedPorts {
		img.Ports = append(img.Ports, p)
	}
	sort.Strings(img.Ports)
	for k, v := range cfg.Config.Labels {
		img.Labels = append(img.Labels, Label{k, v})
	}
	sort.Slice(img.Labels, func(i, j int) bool { return img.Labels[i].Key < img.Labels[j].Key })

	// Pair history steps with layers: every step without empty_layer
	// produced the next layer in order.
	li := 0
	for _, h := range cfg.History {
		row := Layer{Command: cleanCommand(h.CreatedBy)}
		if !h.EmptyLayer && li < len(doc.Layers) {
			row.Digest, row.Size = doc.Layers[li].Digest, doc.Layers[li].Size
			li++
		}
		img.Layers = append(img.Layers, row)
	}
	for ; li < len(doc.Layers); li++ { // no (or short) history
		img.Layers = append(img.Layers, Layer{Digest: doc.Layers[li].Digest, Size: doc.Layers[li].Size})
	}
	return img, nil
}

// cleanCommand turns history "created_by" into the Dockerfile instruction.
func cleanCommand(c string) string {
	c = strings.TrimSpace(c)
	c = strings.TrimSuffix(c, "# buildkit")
	if rest, ok := strings.CutPrefix(c, "/bin/sh -c #(nop)"); ok {
		c = rest
	} else if rest, ok := strings.CutPrefix(c, "/bin/sh -c "); ok {
		c = "RUN " + rest
	}
	return strings.TrimSpace(c)
}

// imageConfig reads and caches (by digest: immutable) an image config blob.
func (s *Service) imageConfig(ctx context.Context, d *Descriptor) (*ImageConfig, error) {
	key := "registry:config:" + d.Digest
	var cfg ImageConfig
	if s.KV != nil {
		if ok, err := s.KV.GetJSON(ctx, key, &cfg); ok && err == nil {
			return &cfg, nil
		}
	}
	if d.Size > 4<<20 {
		return nil, errors.New("image config too large")
	}
	b, err := s.Store.RegistryBlob(ctx, d.Digest)
	if err != nil {
		return nil, err
	}
	rc, _, err := s.Blob.Get(ctx, b.S3Key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	if err := json.NewDecoder(io.LimitReader(rc, 4<<20)).Decode(&cfg); err != nil {
		return nil, err
	}
	if s.KV != nil {
		s.KV.SetJSON(ctx, key, &cfg, 30*24*time.Hour)
	}
	return &cfg, nil
}
