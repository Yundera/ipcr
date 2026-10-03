package main

// import: copy an image from any OCI registry into IPFS, in the exact format `nerdctl push ipfs://`
// writes (spec §4), without containerd. watch: keep doing that for every tag of every repository
// a registry lists.
//
// Byte-for-byte compatibility is the point: the same image imported here or pushed by nerdctl gets
// the same CIDs, so nodes dedupe and re-share it. That means mirroring what nerdctl runs, which is
// containerd's converter (core/images/converter/default.go, docker2oci=true, a platform filter)
// with stargz-snapshotter's hook (ipfs/converter.go) adding each blob with `add?cid-version=1&pin=true`
// and recording `urls: ["ipfs://<cid>"]`:
//
//   - manifests and indexes are decoded into the OCI structs and re-encoded with encoding/json
//     (struct field order below follows image-spec, unknown fields are dropped);
//   - Docker media types become OCI ones, on documents and on descriptors;
//   - index entries whose platform does not match are removed;
//   - layers and configs keep their bytes; only their descriptors gain urls/mediaType;
//   - the root is the JSON of the top descriptor, added like any blob.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------- OCI types (image-spec field order)

type ociPlatform struct {
	Architecture string   `json:"architecture"`
	OS           string   `json:"os"`
	OSVersion    string   `json:"os.version,omitempty"`
	OSFeatures   []string `json:"os.features,omitempty"`
	Variant      string   `json:"variant,omitempty"`
}

type ociDescriptor struct {
	MediaType    string            `json:"mediaType"`
	Digest       string            `json:"digest"`
	Size         int64             `json:"size"`
	URLs         []string          `json:"urls,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	Data         []byte            `json:"data,omitempty"`
	Platform     *ociPlatform      `json:"platform,omitempty"`
	ArtifactType string            `json:"artifactType,omitempty"`
}

type ociManifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType,omitempty"`
	ArtifactType  string            `json:"artifactType,omitempty"`
	Config        ociDescriptor     `json:"config"`
	Layers        []ociDescriptor   `json:"layers"`
	Subject       *ociDescriptor    `json:"subject,omitempty"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

type ociIndex struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType,omitempty"`
	ArtifactType  string            `json:"artifactType,omitempty"`
	Manifests     []ociDescriptor   `json:"manifests"`
	Subject       *ociDescriptor    `json:"subject,omitempty"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

var dockerToOCI = map[string]string{
	"application/vnd.docker.distribution.manifest.list.v2+json": "application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json":      "application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.image.rootfs.diff.tar.gzip":         "application/vnd.oci.image.layer.v1.tar+gzip",
	"application/vnd.docker.image.rootfs.foreign.diff.tar.gzip": "application/vnd.oci.image.layer.nondistributable.v1.tar+gzip",
	"application/vnd.docker.image.rootfs.diff.tar":              "application/vnd.oci.image.layer.v1.tar",
	"application/vnd.docker.image.rootfs.foreign.diff.tar":      "application/vnd.oci.image.layer.nondistributable.v1.tar",
	"application/vnd.docker.container.image.v1+json":            "application/vnd.oci.image.config.v1+json",
}

func toOCI(mt string) string {
	if o, ok := dockerToOCI[mt]; ok {
		return o
	}
	return mt
}

func isIndex(mt string) bool {
	return mt == "application/vnd.oci.image.index.v1+json" || mt == "application/vnd.docker.distribution.manifest.list.v2+json"
}

func isManifest(mt string) bool {
	return mt == "application/vnd.oci.image.manifest.v1+json" || mt == "application/vnd.docker.distribution.manifest.v2+json"
}

const manifestAccept = "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json"

// ---------------------------------------------------------------- registry client

type imageRef struct {
	scheme, host, repo, ref string // ref: tag or digest
}

// parseRef accepts docker-style references, optionally prefixed with http:// or https://.
func parseRef(s string) (imageRef, error) {
	r := imageRef{scheme: "https"}
	if i := strings.Index(s, "://"); i > 0 {
		r.scheme, s = s[:i], s[i+3:]
	}
	first, rest, ok := strings.Cut(s, "/")
	if ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		r.host, s = first, rest
	} else {
		r.host = "registry-1.docker.io"
		if !strings.Contains(s, "/") {
			s = "library/" + s
		}
	}
	if r.host == "docker.io" {
		r.host = "registry-1.docker.io"
	}
	if i := strings.Index(s, "@"); i >= 0 {
		r.repo, r.ref = s[:i], s[i+1:]
	} else if i := strings.LastIndex(s, ":"); i >= 0 {
		r.repo, r.ref = s[:i], s[i+1:]
	} else {
		r.repo, r.ref = s, "latest"
	}
	if r.repo == "" || r.ref == "" {
		return r, fmt.Errorf("bad image reference %q", s)
	}
	return r, nil
}

func (r imageRef) String() string { return r.host + "/" + r.repo + ":" + r.ref }

// registry is a minimal pull client: bearer-token or basic auth. Credentials (user:secret) come
// from the file IMPORT_AUTH_FILE if set — what an install step can write — else from IMPORT_AUTH.
type registry struct {
	scheme, host string
	user, pass   string
	client       *http.Client
	mu           sync.Mutex
	tokens       map[string]string // scope → bearer token
}

func newRegistry(scheme, host string) *registry {
	r := &registry{scheme: scheme, host: host, client: &http.Client{Timeout: 30 * time.Minute}, tokens: map[string]string{}}
	if a := importAuth(); a != "" && credentialsFor(host) {
		r.user, r.pass, _ = strings.Cut(a, ":")
	}
	return r
}

// credentialsFor keeps IMPORT_AUTH scoped: when IMPORT_WATCH names a registry, its credentials go
// to that host only — never to Docker Hub or any other registry an ad-hoc `import` reaches.
func credentialsFor(host string) bool {
	w := os.Getenv("IMPORT_WATCH")
	if w == "" {
		return true
	}
	u, err := url.Parse(w)
	return err == nil && u.Host == host
}

func importAuth() string {
	if f := os.Getenv("IMPORT_AUTH_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			log.Printf("IMPORT_AUTH_FILE: %v", err)
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	return os.Getenv("IMPORT_AUTH")
}

var authParam = regexp.MustCompile(`(\w+)="([^"]*)"`)

func (r *registry) do(ctx context.Context, method, path, accept, scope string) (*http.Response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, r.scheme+"://"+r.host+path, nil)
		if err != nil {
			return nil, err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		r.mu.Lock()
		tok := r.tokens[scope]
		r.mu.Unlock()
		switch {
		case tok == "basic":
			req.SetBasicAuth(r.user, r.pass)
		case tok != "":
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := r.client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusUnauthorized || attempt == 1 {
			if resp.StatusCode >= 300 {
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
				resp.Body.Close()
				return nil, fmt.Errorf("%s %s%s: %s %s", method, r.host, path, resp.Status, strings.TrimSpace(string(b)))
			}
			return resp, nil
		}
		challenge := resp.Header.Get("Www-Authenticate")
		resp.Body.Close()
		if err := r.authenticate(ctx, challenge, scope); err != nil {
			return nil, err
		}
	}
	panic("unreachable")
}

func (r *registry) authenticate(ctx context.Context, challenge, scope string) error {
	if !strings.HasPrefix(strings.ToLower(challenge), "bearer") {
		if r.user == "" {
			return fmt.Errorf("%s wants basic auth: set IMPORT_AUTH=user:secret", r.host)
		}
		r.mu.Lock()
		r.tokens[scope] = "basic"
		r.mu.Unlock()
		return nil
	}
	p := map[string]string{}
	// First value wins: a header may offer a second scheme after Bearer
	// (Gitea: `Bearer realm="…",service="…", Basic realm="Gitea Container Registry"`).
	for _, m := range authParam.FindAllStringSubmatch(challenge, -1) {
		if k := strings.ToLower(m[1]); p[k] == "" {
			p[k] = m[2]
		}
	}
	u, err := url.Parse(p["realm"])
	if err != nil || p["realm"] == "" {
		return fmt.Errorf("bad auth challenge from %s: %q", r.host, challenge)
	}
	q := u.Query()
	if p["service"] != "" {
		q.Set("service", p["service"])
	}
	q.Set("scope", scope)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return err
	}
	if r.user != "" {
		req.SetBasicAuth(r.user, r.pass)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token for %s on %s: %s (set IMPORT_AUTH=user:secret?)", scope, r.host, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return err
	}
	if t.Token == "" {
		t.Token = t.AccessToken
	}
	r.mu.Lock()
	r.tokens[scope] = t.Token
	r.mu.Unlock()
	return nil
}

func pullScope(repo string) string { return "repository:" + repo + ":pull" }

// manifest fetches a manifest/index by tag or digest and verifies it when fetched by digest.
func (r *registry) manifest(ctx context.Context, repo, ref string) ([]byte, string, error) {
	resp, err := r.do(ctx, "GET", "/v2/"+repo+"/manifests/"+ref, manifestAccept, pullScope(repo))
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, "", err
	}
	if strings.HasPrefix(ref, "sha256:") && sha256Digest(b) != ref {
		return nil, "", fmt.Errorf("manifest %s@%s: digest mismatch", repo, ref)
	}
	mt, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	if mt == "" || mt == "application/json" || mt == "text/plain" {
		var probe struct{ MediaType string }
		_ = json.Unmarshal(b, &probe)
		mt = probe.MediaType
	}
	return b, mt, nil
}

// headDigest returns the manifest digest a tag currently points to.
func (r *registry) headDigest(ctx context.Context, repo, ref string) (string, error) {
	resp, err := r.do(ctx, "HEAD", "/v2/"+repo+"/manifests/"+ref, manifestAccept, pullScope(repo))
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	if d := resp.Header.Get("Docker-Content-Digest"); d != "" {
		return d, nil
	}
	b, _, err := r.manifest(ctx, repo, ref)
	return sha256Digest(b), err
}

func (r *registry) blob(ctx context.Context, repo, dgst string) (io.ReadCloser, error) {
	resp, err := r.do(ctx, "GET", "/v2/"+repo+"/blobs/"+dgst, "", pullScope(repo))
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (r *registry) catalog(ctx context.Context) ([]string, error) {
	resp, err := r.do(ctx, "GET", "/v2/_catalog?n=1000", "", "registry:catalog:*")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var c struct{ Repositories []string }
	return c.Repositories, json.NewDecoder(resp.Body).Decode(&c)
}

func (r *registry) tags(ctx context.Context, repo string) ([]string, error) {
	resp, err := r.do(ctx, "GET", "/v2/"+repo+"/tags/list", "", pullScope(repo))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var t struct{ Tags []string }
	return t.Tags, json.NewDecoder(resp.Body).Decode(&t)
}

func sha256Digest(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// ---------------------------------------------------------------- IPFS add (stargz client parameters)

func kuboAdd(ctx context.Context, r io.Reader) (string, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		fw, err := mw.CreateFormFile("file", "file")
		if err == nil {
			_, err = io.Copy(fw, r)
		}
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()
	req, err := http.NewRequestWithContext(ctx, "POST", kuboAPI+"/api/v0/add?cid-version=1&pin=true", pr)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := kuboClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("kubo add: %s %s", resp.Status, b)
	}
	var out struct{ Hash string }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Hash == "" {
		return "", fmt.Errorf("kubo add: empty hash")
	}
	return out.Hash, nil
}

// ---------------------------------------------------------------- conversion

type importer struct {
	reg      *registry
	repo     string
	platform ociPlatform
	blobs    int
}

// platformMatch mirrors containerd's default matcher closely enough for linux images: same OS and
// architecture, and for arm the same variant (an absent variant matches the default one).
func (im *importer) platformMatch(p *ociPlatform) bool {
	if p.OS != im.platform.OS || p.Architecture != im.platform.Architecture {
		return false
	}
	norm := func(v string) string {
		if v == "" && p.Architecture == "arm64" {
			return "v8"
		}
		return v
	}
	return im.platform.Variant == "" || norm(p.Variant) == norm(im.platform.Variant)
}

// convert returns the descriptor as it appears in the converted image (urls + OCI media type),
// after adding the converted content to IPFS.
func (im *importer) convert(ctx context.Context, d ociDescriptor) (ociDescriptor, error) {
	var content []byte // set for rewritten documents; blobs are streamed
	switch {
	case isIndex(d.MediaType):
		raw, _, err := im.reg.manifest(ctx, im.repo, d.Digest)
		if err != nil {
			return d, err
		}
		var idx ociIndex
		if err := json.Unmarshal(raw, &idx); err != nil {
			return d, err
		}
		idx.MediaType = toOCI(idx.MediaType)
		kept := []ociDescriptor{}
		for _, m := range idx.Manifests {
			if m.Platform != nil && !im.platformMatch(m.Platform) {
				continue
			}
			nm, err := im.convert(ctx, m)
			if err != nil {
				return d, err
			}
			kept = append(kept, nm)
		}
		idx.Manifests = kept
		if content, err = json.Marshal(&idx); err != nil {
			return d, err
		}
	case isManifest(d.MediaType):
		raw, _, err := im.reg.manifest(ctx, im.repo, d.Digest)
		if err != nil {
			return d, err
		}
		var m ociManifest
		if err := json.Unmarshal(raw, &m); err != nil {
			return d, err
		}
		m.MediaType = toOCI(m.MediaType)
		for i, l := range m.Layers {
			if m.Layers[i], err = im.convert(ctx, l); err != nil {
				return d, err
			}
		}
		if m.Config, err = im.convert(ctx, m.Config); err != nil {
			return d, err
		}
		if content, err = json.Marshal(&m); err != nil {
			return d, err
		}
	}

	var cid string
	var err error
	if content != nil {
		d.Digest, d.Size = sha256Digest(content), int64(len(content))
		cid, err = kuboAdd(ctx, strings.NewReader(string(content)))
	} else {
		cid, err = im.addBlob(ctx, d)
	}
	if err != nil {
		return d, err
	}
	im.blobs++
	d.URLs = []string{"ipfs://" + cid}
	d.MediaType = toOCI(d.MediaType)
	return d, nil
}

// addBlob streams a layer or config from the registry into IPFS, checking its digest on the way.
func (im *importer) addBlob(ctx context.Context, d ociDescriptor) (string, error) {
	body, err := im.reg.blob(ctx, im.repo, d.Digest)
	if err != nil {
		return "", err
	}
	defer body.Close()
	h := sha256.New()
	n := int64(0)
	cid, err := kuboAdd(ctx, io.TeeReader(&countReader{body, &n}, h))
	if err != nil {
		return "", err
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != d.Digest || n != d.Size {
		// Unpin what was just added: it is not the blob the manifest names.
		_ = kuboJSON(ctx, "pin/rm", arg(cid), nil)
		return "", fmt.Errorf("blob %s: got %s (%d bytes), want %d bytes", d.Digest, got, n, d.Size)
	}
	return cid, nil
}

type countReader struct {
	r io.Reader
	n *int64
}

func (c *countReader) Read(p []byte) (int, error) {
	k, err := c.r.Read(p)
	*c.n += int64(k)
	return k, err
}

func defaultPlatform() ociPlatform {
	p := ociPlatform{OS: "linux", Architecture: runtime.GOARCH}
	if s := os.Getenv("IMPORT_PLATFORM"); s != "" {
		parts := strings.Split(s, "/")
		p = ociPlatform{OS: parts[0]}
		if len(parts) > 1 {
			p.Architecture = parts[1]
		}
		if len(parts) > 2 {
			p.Variant = parts[2]
		}
	}
	return p
}

// importImage copies ref into IPFS and returns the image root CID.
func importImage(ctx context.Context, ref string) (string, error) {
	r, err := parseRef(ref)
	if err != nil {
		return "", err
	}
	return importFrom(ctx, newRegistry(r.scheme, r.host), r.repo, r.ref)
}

func importFrom(ctx context.Context, reg *registry, repo, ref string) (string, error) {
	start := time.Now()
	raw, mt, err := reg.manifest(ctx, repo, ref)
	if err != nil {
		return "", err
	}
	im := &importer{reg: reg, repo: repo, platform: defaultPlatform()}
	top, err := im.convert(ctx, ociDescriptor{MediaType: mt, Digest: sha256Digest(raw), Size: int64(len(raw))})
	if err != nil {
		return "", err
	}
	rootJSON, err := json.Marshal(&top)
	if err != nil {
		return "", err
	}
	root, err := kuboAdd(ctx, strings.NewReader(string(rootJSON)))
	if err != nil {
		return "", err
	}
	sep := ":"
	if strings.HasPrefix(ref, "sha256:") {
		sep = "@"
	}
	log.Printf("imported %s/%s%s%s → %s (%d objects, %s)", reg.host, repo, sep, ref, root, im.blobs+1, time.Since(start).Round(time.Second))
	return root, nil
}

// ---------------------------------------------------------------- watch

// watch polls a registry and imports every tag of every repository it lists (or of the
// repositories named in IMPORT_REPOS), publishing each as /ipns/<key>/<tag>, where <key> is the
// repository path with "/" → "-". A tag is re-imported when its manifest digest changes, which is
// how a moving tag like `latest` follows the registry.
func watch(ctx context.Context, registryURL string, every time.Duration, stateDir string) {
	u, err := url.Parse(registryURL)
	if err != nil || u.Host == "" {
		log.Printf("watch: bad IMPORT_WATCH %q", registryURL)
		return
	}
	statePath := filepath.Join(stateDir, "imported.json")
	state := map[string]string{} // "repo:tag" → manifest digest last imported
	if b, err := os.ReadFile(statePath); err == nil {
		_ = json.Unmarshal(b, &state)
	}
	// published.json is the public view of the same thing — what to write in an `image:` line —
	// for a UI to read (the forge's landing page serves it as /images.json).
	pubPath := filepath.Join(stateDir, "published.json")
	pub := published{Images: map[string]*publishedRepo{}}
	if b, err := os.ReadFile(pubPath); err == nil {
		_ = json.Unmarshal(b, &pub)
	}
	write := func(path string, v any) {
		b, _ := json.MarshalIndent(v, "", "  ")
		err := os.MkdirAll(stateDir, 0o755)
		if err == nil {
			err = os.WriteFile(path+".tmp", b, 0o644)
		}
		if err == nil {
			err = os.Rename(path+".tmp", path)
		}
		if err != nil {
			log.Printf("watch: save %s: %v", filepath.Base(path), err)
		}
	}
	log.Printf("watch: %s every %s", registryURL, every)
	for {
		// A fresh client each pass: credentials are re-read (a token written or rotated by an
		// install step is picked up) and cached bearer tokens never outlive their expiry.
		reg := newRegistry(u.Scheme, u.Host)
		repos := strings.FieldsFunc(os.Getenv("IMPORT_REPOS"), func(r rune) bool { return r == ',' || r == ' ' })
		if len(repos) == 0 {
			if repos, err = reg.catalog(ctx); err != nil {
				log.Printf("watch: catalog: %v", err)
			}
		}
		for _, repo := range repos {
			tags, err := reg.tags(ctx, repo)
			if err != nil {
				log.Printf("watch: %s: %v", repo, err)
				continue
			}
			for _, tag := range tags {
				dgst, err := reg.headDigest(ctx, repo, tag)
				if err != nil {
					log.Printf("watch: %s:%s: %v", repo, tag, err)
					continue
				}
				key := repo + ":" + tag
				if state[key] == dgst && pub.has(repo, tag) {
					continue
				}
				root, err := importFrom(ctx, reg, repo, dgst)
				if err == nil {
					var name string
					name, err = tagImage(ctx, strings.ReplaceAll(repo, "/", "-"), tag, root)
					if err == nil {
						log.Printf("watch: %s:%s → ipcr.localhost:4767/ipns/%s:%s (/ipfs/%s)", repo, tag, name, tag, root)
						pub.set(repo, name, tag, publishedTag{CID: root, Digest: dgst, At: time.Now().UTC().Format(time.RFC3339)})
						write(pubPath, &pub)
					}
				}
				if err != nil {
					log.Printf("watch: %s:%s: %v", repo, tag, err)
					continue
				}
				state[key] = dgst
				write(statePath, state)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

type publishedTag struct {
	CID    string `json:"cid"`
	Digest string `json:"digest"`
	At     string `json:"at"`
}

type publishedRepo struct {
	IPNS string                  `json:"ipns"`
	Tags map[string]publishedTag `json:"tags"`
}

type published struct {
	Registry string                    `json:"registry"`
	Images   map[string]*publishedRepo `json:"images"`
}

func (p *published) has(repo, tag string) bool {
	r := p.Images[repo]
	if r == nil {
		return false
	}
	_, ok := r.Tags[tag]
	return ok
}

func (p *published) set(repo, ipns, tag string, t publishedTag) {
	p.Registry = "ipcr.localhost:4767"
	r := p.Images[repo]
	if r == nil {
		r = &publishedRepo{Tags: map[string]publishedTag{}}
		p.Images[repo] = r
	}
	r.IPNS = ipns
	r.Tags[tag] = t
}
