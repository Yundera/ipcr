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
	"crypto/rand"
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
	"sort"
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

// ensureImportAuth creates IMPORT_AUTH_FILE (user ipcr, a random password) when it is missing and
// IMPORT_AUTH_GENERATE=true: the forge's staging registry gate learns it from that file (read-only
// mount), so this side never needs to be told a secret. World-readable for the same reason as
// admin-token (admin.go): root without capabilities cannot hand a file to another uid otherwise.
func ensureImportAuth() error {
	f := os.Getenv("IMPORT_AUTH_FILE")
	if f == "" || os.Getenv("IMPORT_AUTH_GENERATE") != "true" {
		return nil
	}
	if b, err := os.ReadFile(f); err == nil && strings.Contains(string(b), ":") {
		return nil
	}
	b := make([]byte, 24)
	rand.Read(b)
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(f+".tmp", []byte("ipcr:"+hex.EncodeToString(b)), 0o644); err != nil {
		return err
	}
	return os.Rename(f+".tmp", f)
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

// deleteManifest removes a manifest (and with it every tag pointing at it) by digest. The registry
// must allow deletes; the blobs go at its next garbage collection.
func (r *registry) deleteManifest(ctx context.Context, repo, dgst string) error {
	resp, err := r.do(ctx, "DELETE", "/v2/"+repo+"/manifests/"+dgst, "", "repository:"+repo+":delete")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
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
// IMPORT_PLATFORM=all keeps every entry, like `nerdctl push --all-platforms` (platforms.All).
func (im *importer) platformMatch(p *ociPlatform) bool {
	if im.platform.OS == "all" {
		return true
	}
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
	announce(ctx, root)
	return root, nil
}

// announce puts a provider record for root in the DHT now. Kubo's own reprovider gets to new
// content lazily (much later in the lowpower profile), and until then another node's first pull
// spends minutes finding out who has the root. Only the root is needed: once a peer has found
// this node for it, bitswap fetches every other block from the same connection.
func announce(ctx context.Context, root string) {
	start := time.Now()
	if err := kuboJSON(ctx, "routing/provide", url.Values{"arg": {root}}, nil); err != nil {
		log.Printf("announce %s: %v", root, err)
		return
	}
	log.Printf("announced %s (%s)", root, time.Since(start).Round(time.Second))
}

// ---------------------------------------------------------------- watch

// watcher polls a registry and imports every tag of every repository it lists (or of the
// repositories named in IMPORT_REPOS), publishing each as /ipns/<key>/<tag>, where <key> is the
// repository path with "/" → "-". A tag is re-imported when its manifest digest changes, which is
// how a moving tag like `latest` follows the registry.
//
// With IMPORT_PUBLISHER=<key name>, every repository is published under that one key instead, as
// /ipns/<key>/<repo>/<tag>: one name for all the images, which a DNSLink domain or an ENS name
// can point at (ipcr.localhost:4767/ipns/example.eth/<repo>:<tag>).
//
// Its state is shared with the admin API (admin.go), hence the mutex: the API unpublishes, moves
// tags and asks for re-imports, and reads what is published.
type watcher struct {
	mu        sync.Mutex
	u         *url.URL
	every     time.Duration
	stateDir  string
	publisher string
	state     map[string]string // state key (see stateKey) → manifest digest last imported, or tombstone
	pub       published
	kick      chan struct{}
	skipped   map[string]bool // repositories already logged as not allowed
}

// The running watcher, for the admin API. nil when IMPORT_WATCH is not set.
var theWatcher *watcher

// A tag the admin unpublished: state[key] = tombstone + digest. The watcher leaves it alone until the
// registry's tag moves to another digest.
const tombstone = "unpublished:"

func newWatcher(registryURL string, every time.Duration, stateDir string) (*watcher, error) {
	u, err := url.Parse(registryURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("bad IMPORT_WATCH %q", registryURL)
	}
	w := &watcher{u: u, every: every, stateDir: stateDir, publisher: os.Getenv("IMPORT_PUBLISHER"),
		state: map[string]string{}, pub: published{Images: map[string]*publishedRepo{}},
		kick: make(chan struct{}, 1), skipped: map[string]bool{}}
	if b, err := os.ReadFile(w.path("imported.json")); err == nil {
		_ = json.Unmarshal(b, &w.state)
	}
	// published.json is the public view of the same thing — what to write in an `image:` line —
	// for a UI to read (the forge's page serves it as /images.json).
	if b, err := os.ReadFile(w.path("published.json")); err == nil {
		_ = json.Unmarshal(b, &w.pub)
	}
	if w.pub.Images == nil {
		w.pub.Images = map[string]*publishedRepo{}
	}
	return w, nil
}

func (w *watcher) path(name string) string { return filepath.Join(w.stateDir, name) }

func (w *watcher) stateKey(repo, tag string) string {
	if w.publisher != "" {
		// Its own entries, so that turning publisher mode on publishes what is there.
		return w.publisher + "|" + repo + ":" + tag
	}
	return repo + ":" + tag
}

// save writes the watcher's two files. Call with w.mu held.
func (w *watcher) save() {
	writeJSON(w.path("imported.json"), w.state)
	writeJSON(w.path("published.json"), &w.pub)
}

// writeJSON writes v to path atomically (written then renamed, so a reader never sees half of it).
func writeJSON(path string, v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	err := os.MkdirAll(filepath.Dir(path), 0o755)
	if err == nil {
		err = os.WriteFile(path+".tmp", b, 0o644)
	}
	if err == nil {
		err = os.Rename(path+".tmp", path)
	}
	if err != nil {
		log.Printf("save %s: %v", filepath.Base(path), err)
	}
}

// wake asks for a pass now rather than at the next interval.
func (w *watcher) wake() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

func (w *watcher) run(ctx context.Context) {
	log.Printf("watch: %s every %s", w.u, w.every)
	if w.publisher != "" {
		log.Printf("watch: publishing every repository under the IPNS key %q", w.publisher)
		go w.ensurePublisherKey(ctx, 5*time.Second)
	}
	for {
		w.pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(w.every):
		case <-w.kick:
		}
	}
}

// ensurePublisherKey creates the publisher's IPNS key at start-up rather than at the first import,
// so its name (k51…) is known from the first boot: an ENS or DNS record can point at it, and the
// key can be backed up, before anything is published. The name resolves to nothing until then.
// Retries until Kubo answers (it may start after this process).
func (w *watcher) ensurePublisherKey(ctx context.Context, retry time.Duration) {
	for {
		err := ensureKey(ctx, w.publisher)
		if err == nil {
			if id, err := keyID(ctx, w.publisher); err == nil && id != "" {
				log.Printf("watch: IPNS key %q is /ipns/%s", w.publisher, id)
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
	}
}

// pass is one look at the registry.
func (w *watcher) pass(ctx context.Context) {
	cfg := loadConfig(w.stateDir)
	// A fresh client each pass: credentials are re-read (a token written or rotated by an install
	// step is picked up) and cached bearer tokens never outlive their expiry.
	reg := newRegistry(w.u.Scheme, w.u.Host)
	repos := strings.FieldsFunc(os.Getenv("IMPORT_REPOS"), func(r rune) bool { return r == ',' || r == ' ' })
	if len(repos) == 0 {
		var err error
		if repos, err = reg.catalog(ctx); err != nil {
			log.Printf("watch: catalog: %v", err)
			return
		}
	}
	for _, repo := range repos {
		if !cfg.allowed(repo) {
			w.mu.Lock()
			if !w.skipped[repo] {
				log.Printf("watch: %s: not on the allowlist, not published", repo)
				w.skipped[repo] = true
			}
			w.mu.Unlock()
			continue
		}
		w.mu.Lock()
		delete(w.skipped, repo)
		w.mu.Unlock()
		tags, err := reg.tags(ctx, repo)
		if err != nil {
			log.Printf("watch: %s: %v", repo, err)
			continue
		}
		digests := map[string]string{} // tag → digest, for the cleanup below
		done := map[string]bool{}      // tag → nothing left to do for it
		complete := true               // every tag's digest is known
		for _, tag := range tags {
			dgst, err := reg.headDigest(ctx, repo, tag)
			if err != nil {
				log.Printf("watch: %s:%s: %v", repo, tag, err)
				complete = false
				continue
			}
			digests[tag] = dgst
			done[tag] = w.importTag(ctx, reg, cfg, repo, tag, dgst)
		}
		if cleanupStaging && complete {
			w.cleanup(ctx, reg, repo, digests, done)
		}
	}
	if w.publisher != "" {
		w.syncAliases(ctx, cfg)
	}
	w.syncPublic(cfg)
}

// syncPublic mirrors config.json's public switch into published.json.
func (w *watcher) syncPublic(cfg config) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pub.Public != cfg.publicOn() {
		w.pub.Public = cfg.publicOn()
		w.save()
	}
}

// importTag imports and publishes repo:tag unless it already is (or was unpublished on purpose).
// It reports whether the tag is settled.
func (w *watcher) importTag(ctx context.Context, reg *registry, cfg config, repo, tag, dgst string) bool {
	key := w.stateKey(repo, tag)
	w.mu.Lock()
	prev, has := w.state[key], w.pub.has(repo, tag)
	w.mu.Unlock()
	if prev == tombstone+dgst || (prev == dgst && has) {
		return true
	}
	root, err := importFrom(ctx, reg, repo, dgst)
	var name string
	if err == nil {
		if w.publisher != "" {
			// Under its own path and, for the root organisation's repositories, its short one.
			w.mu.Lock()
			aliases := w.aliases(cfg)[repo]
			w.mu.Unlock()
			name, err = publishPaths(ctx, w.publisher, append([]string{repo}, aliases...), tag, root)
			if err == nil {
				name += "/" + repo
			}
		} else {
			name, err = tagImage(ctx, strings.ReplaceAll(repo, "/", "-"), tag, root)
		}
	}
	if err != nil {
		log.Printf("watch: %s:%s: %v", repo, tag, err)
		return false
	}
	log.Printf("watch: %s:%s → ipcr.localhost:4767/ipns/%s:%s (/ipfs/%s)", repo, tag, name, tag, root)
	w.mu.Lock()
	if w.publisher != "" {
		w.pub.Publisher, _, _ = strings.Cut(name, "/")
	}
	w.pub.set(repo, name, tag, publishedTag{CID: root, Digest: dgst, At: time.Now().UTC().Format(time.RFC3339)})
	w.state[key] = dgst
	w.save()
	w.mu.Unlock()
	return true
}

// Delete what was imported from the source registry (IMPORT_CLEANUP=true; the forge's staging
// registry, which is only a hand-off). A manifest is deleted by digest, which removes every tag
// pointing at it, so a digest goes only when all of its tags are settled.
var cleanupStaging = env("IMPORT_CLEANUP", "false") == "true"

func (w *watcher) cleanup(ctx context.Context, reg *registry, repo string, digests map[string]string, done map[string]bool) {
	ready := map[string]bool{}
	for tag, d := range digests {
		if _, seen := ready[d]; !seen {
			ready[d] = true
		}
		ready[d] = ready[d] && done[tag]
	}
	for d, ok := range ready {
		if !ok {
			continue
		}
		if err := reg.deleteManifest(ctx, repo, d); err != nil {
			log.Printf("watch: cleanup %s@%s: %v", repo, d, err)
			continue
		}
		log.Printf("watch: cleanup: removed %s@%s from %s", repo, d, reg.host)
	}
}

// aliases returns the extra paths each repository is published under (config allow.aliases), less
// any that would collide: an alias is one path segment at the root of the publisher tree, so it must
// not be the first segment of another repository's path (an owner's folder), nor be claimed twice.
// Call with w.mu held.
func (w *watcher) aliases(cfg config) map[string][]string {
	taken := map[string]string{} // first segment → repository using it
	claim := func(seg, repo string) {
		if _, ok := taken[seg]; !ok {
			taken[seg] = repo
		}
	}
	for _, r := range cfg.Allow.Repos {
		claim(strings.SplitN(r, "/", 2)[0], r)
	}
	for r := range w.pub.Images {
		claim(strings.SplitN(r, "/", 2)[0], r)
	}
	out := map[string][]string{}
	used := map[string]bool{}
	repos := make([]string, 0, len(cfg.Allow.Aliases))
	for r := range cfg.Allow.Aliases {
		repos = append(repos, r)
	}
	sort.Strings(repos)
	for _, repo := range repos {
		for _, a := range cfg.Allow.Aliases[repo] {
			a = strings.ToLower(a)
			if !validAlias(a) || used[a] || taken[a] != "" {
				continue
			}
			used[a] = true
			out[strings.ToLower(repo)] = append(out[strings.ToLower(repo)], a)
		}
	}
	return out
}

// validAlias: one segment of an image path (see repoRe in admin.go).
func validAlias(a string) bool { return a != "" && !strings.Contains(a, "/") && repoRe.MatchString(a) }

// syncAliases makes every published repository's short paths match the configuration: a new alias
// gets every published tag, a dropped one (root organisation changed or switched off) is removed.
func (w *watcher) syncAliases(ctx context.Context, cfg config) {
	w.mu.Lock()
	want := w.aliases(cfg)
	type change struct {
		repo      string
		add, drop []string
		tags      map[string]string // tag → root
	}
	var changes []change
	for repo, pr := range w.pub.Images {
		c := change{repo: repo, tags: map[string]string{}}
		have := map[string]bool{}
		for _, a := range pr.Aliases {
			have[a] = true
		}
		for _, a := range want[repo] {
			if !have[a] {
				c.add = append(c.add, a)
			}
			delete(have, a)
		}
		for a := range have {
			c.drop = append(c.drop, a)
		}
		if len(c.add)+len(c.drop) == 0 {
			continue
		}
		for tag, t := range pr.Tags {
			c.tags[tag] = t.CID
		}
		changes = append(changes, c)
	}
	w.mu.Unlock()
	for _, c := range changes {
		_, err := updateTree(ctx, w.publisher, publisherDir(w.publisher), 0, func(base string) error {
			for _, a := range c.drop {
				if err := kuboJSON(ctx, "files/rm", url.Values{"arg": {base + "/" + a}, "recursive": {"true"}, "force": {"true"}}, nil); err != nil {
					return err
				}
			}
			for _, a := range c.add {
				for tag, root := range c.tags {
					if err := setTag(ctx, base+"/"+a, tag, root); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			log.Printf("watch: aliases of %s: %v", c.repo, err)
			continue
		}
		log.Printf("watch: %s: short paths %v added, %v removed", c.repo, c.add, c.drop)
		w.mu.Lock()
		if pr := w.pub.Images[c.repo]; pr != nil {
			pr.Aliases = want[c.repo]
		}
		w.save()
		w.mu.Unlock()
	}
}

// ---------------------------------------------------------------- configuration

// config is what the admin can change at run time, kept in STATE_DIR/config.json and re-read at
// every pass. Environment variables are the defaults.
type config struct {
	// The DNSLink domain or ENS name pointing at the publisher key, e.g. example.eth. Shown in
	// pull lines once checked (see admin.go).
	Name  string `json:"name,omitempty"`
	Allow struct {
		// Only the listed repositories are published. Also on with IMPORT_ALLOW_REQUIRED=true,
		// where an empty list therefore publishes nothing.
		Required bool     `json:"required"`
		Repos    []string `json:"repos"`
		// Extra, shorter paths for some repositories: {"metadec/app": ["app"]} also publishes
		// metadec/app as /ipns/<publisher>/app (IPCR Forge: its root organisation's repositories).
		Aliases map[string][]string `json:"aliases,omitempty"`
	} `json:"allow"`
	// The public front door (public.go): nil or true = on.
	Public *bool `json:"public,omitempty"`
}

func (c config) publicOn() bool { return c.Public == nil || *c.Public }

func loadConfig(stateDir string) config {
	var c config
	if b, err := os.ReadFile(filepath.Join(stateDir, "config.json")); err == nil {
		if err := json.Unmarshal(b, &c); err != nil {
			log.Printf("config.json: %v", err)
		}
	}
	return c
}

func (c config) allowed(repo string) bool {
	if !c.Allow.Required && os.Getenv("IMPORT_ALLOW_REQUIRED") != "true" && len(c.Allow.Repos) == 0 {
		return true
	}
	for _, r := range c.Allow.Repos {
		if strings.EqualFold(r, repo) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- published.json

type publishedTag struct {
	CID    string `json:"cid"`
	Digest string `json:"digest"`
	At     string `json:"at"`
}

type publishedRepo struct {
	IPNS string                  `json:"ipns"`
	Tags map[string]publishedTag `json:"tags"`
	// Short paths the same tags are also published under (see config.Allow.Aliases).
	Aliases []string `json:"aliases,omitempty"`
}

type published struct {
	Registry string `json:"registry"`
	// The IPNS name of the IMPORT_PUBLISHER key, when set: what a DNSLink or ENS record points at.
	Publisher string `json:"publisher,omitempty"`
	// The DNSLink domain or ENS name configured for the publisher, set only while it is checked
	// to resolve to Publisher: pull lines can use it instead of the k51 name.
	Name           string `json:"name,omitempty"`
	NameVerifiedAt string `json:"nameVerifiedAt,omitempty"`
	// Whether the public front door serves these images (config.json "public"), for pages to show
	// its pull lines.
	Public bool                      `json:"public"`
	Images map[string]*publishedRepo `json:"images"`
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

func (p *published) remove(repo, tag string) {
	if r := p.Images[repo]; r != nil {
		delete(r.Tags, tag)
		if len(r.Tags) == 0 {
			delete(p.Images, repo)
		}
	}
}
