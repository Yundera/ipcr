// ipcrd — IPCR gateway, a thin layer over `nerdctl ipfs registry serve`.
//
//	serve                     registry front door: /v2/ipfs/<cid> passthrough + /v2/ipns/<name>:<tag>
//	pin <rootCID>...          pin an nerdctl IPFS image *and every blob it references*
//	tag <app> <tag> <rootCID> add a tag to the app's tag directory (MFS) and publish it under IPNS key <app>
//	resolve <name> [tag]      print the root CID a name/tag resolves to
//	health [url]              exit 0 if url (default: this gateway's /v2/) answers 200 — for healthchecks
//
// nerdctl stores each blob as its own IPFS object and links them only via `urls: ["ipfs://…"]`
// inside the JSON, so pinning the root CID alone leaves the layers collectable. `pin` walks them.
//
// IPNS layout: /ipns/<name> → UnixFS directory whose entries are tags, each entry being an image
// root CID (as printed by `nerdctl push ipfs://`). <name> is an IPNS key (k51…) or a DNSLink domain.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	kuboAPI  = env("KUBO_API", "http://ipcr-kubo:5001")
	upstream = env("REGISTRY_UPSTREAM", "http://ipcr-registry:5050")
	listen   = env("LISTEN", ":4767")
	autoPin  = env("AUTO_PIN", "true") == "true"
	tlsDir   = env("TLS_DIR", "") // where the CA + leaf live; empty = plain HTTP
	hostName = env("REGISTRY_HOST", "ipcr.localhost:4767")
	caExport = env("CA_EXPORT", "") // file the CA is copied to, e.g. host /etc/docker/certs.d/ipcr.localhost:4767/ca.crt
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	if len(os.Args) < 2 {
		usage()
	}
	ctx := context.Background()
	var err error
	switch args := os.Args[2:]; os.Args[1] {
	case "serve":
		err = serve()
	case "pin":
		for _, c := range args {
			if err = pinImage(ctx, c); err != nil {
				break
			}
		}
	case "tag":
		if len(args) != 3 {
			usage()
		}
		var name string
		if name, err = tagImage(ctx, args[0], args[1], args[2]); err == nil {
			fmt.Println(name)
		}
	case "health":
		u := "https://127.0.0.1" + listen + "/v2/"
		if len(args) > 0 {
			u = args[0]
		}
		err = health(u)
	case "resolve":
		if len(args) < 1 {
			usage()
		}
		tag := "latest"
		if len(args) > 1 {
			tag = args[1]
		}
		var c string
		if c, err = resolveTag(ctx, args[0], tag); err == nil {
			fmt.Println(c)
		}
	default:
		usage()
	}
	if err != nil {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: ipcrd serve | pin <cid>... | tag <app> <tag> <cid> | resolve <name> [tag] | health [url]")
	os.Exit(2)
}

// ---------------------------------------------------------------- kubo RPC

var kuboClient = &http.Client{Timeout: 10 * time.Minute}

func kubo(ctx context.Context, cmd string, args url.Values) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", kuboAPI+"/api/v0/"+cmd+"?"+args.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := kuboClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		var e struct{ Message string }
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if json.Unmarshal(b, &e) != nil || e.Message == "" {
			e.Message = strings.TrimSpace(string(b))
		}
		return nil, fmt.Errorf("kubo %s %v: %s", cmd, args["arg"], e.Message)
	}
	return resp.Body, nil
}

func kuboJSON(ctx context.Context, cmd string, args url.Values, out any) error {
	body, err := kubo(ctx, cmd, args)
	if err != nil {
		return err
	}
	defer body.Close()
	if out == nil {
		_, err = io.Copy(io.Discard, body)
		return err
	}
	return json.NewDecoder(body).Decode(out)
}

func kuboCat(ctx context.Context, cid string) ([]byte, error) {
	body, err := kubo(ctx, "cat", url.Values{"arg": {cid}})
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return io.ReadAll(io.LimitReader(body, 8<<20)) // descriptors/manifests/indexes only
}

func arg(a ...string) url.Values { return url.Values{"arg": a} }

// ---------------------------------------------------------------- pinning

type descriptor struct {
	MediaType string   `json:"mediaType"`
	Digest    string   `json:"digest"`
	URLs      []string `json:"urls"`
}

func cidOf(d descriptor) string {
	for _, u := range d.URLs {
		if strings.HasPrefix(u, "ipfs://") {
			return u[len("ipfs://"):]
		}
	}
	return ""
}

func hasChildren(mediaType string) bool {
	return strings.Contains(mediaType, "manifest") || strings.Contains(mediaType, "index")
}

func pinImage(ctx context.Context, root string) error {
	raw, err := kuboCat(ctx, root)
	if err != nil {
		return err
	}
	var d descriptor
	if err := json.Unmarshal(raw, &d); err != nil {
		return fmt.Errorf("%s is not an nerdctl IPFS image root: %w", root, err)
	}
	if err := kuboJSON(ctx, "pin/add", arg(root), nil); err != nil {
		return err
	}
	n, err := pinDesc(ctx, d)
	if err == nil {
		log.Printf("pinned %s (%d objects)", root, n+1)
	}
	return err
}

func pinDesc(ctx context.Context, d descriptor) (int, error) {
	c := cidOf(d)
	if c == "" {
		// e.g. other platforms of a multi-arch index that were not pushed
		log.Printf("skip %s %s: no ipfs:// url", d.MediaType, d.Digest)
		return 0, nil
	}
	if err := kuboJSON(ctx, "pin/add", arg(c), nil); err != nil {
		return 0, err
	}
	n := 1
	if !hasChildren(d.MediaType) {
		return n, nil
	}
	raw, err := kuboCat(ctx, c)
	if err != nil {
		return n, err
	}
	var m struct {
		Manifests []descriptor `json:"manifests"`
		Config    *descriptor  `json:"config"`
		Layers    []descriptor `json:"layers"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return n, err
	}
	children := append(m.Manifests, m.Layers...)
	if m.Config != nil {
		children = append(children, *m.Config)
	}
	for _, ch := range children {
		k, err := pinDesc(ctx, ch)
		n += k
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

var pinned sync.Map

func autoPinOnce(root string) {
	if !autoPin {
		return
	}
	if _, dup := pinned.LoadOrStore(root, true); dup {
		return
	}
	go func() {
		if err := pinImage(context.Background(), root); err != nil {
			log.Printf("auto-pin %s: %v", root, err)
			pinned.Delete(root)
		}
	}()
}

// ---------------------------------------------------------------- tags / IPNS

func tagImage(ctx context.Context, app, tag, root string) (string, error) {
	dir := "/images/" + app
	if err := kuboJSON(ctx, "files/mkdir", url.Values{"arg": {dir}, "parents": {"true"}}, nil); err != nil {
		return "", err
	}
	_ = kuboJSON(ctx, "files/rm", url.Values{"arg": {dir + "/" + tag}, "force": {"true"}}, nil)
	if err := kuboJSON(ctx, "files/cp", arg("/ipfs/"+root, dir+"/"+tag), nil); err != nil {
		return "", err
	}
	var st struct{ Hash string }
	if err := kuboJSON(ctx, "files/stat", url.Values{"arg": {dir}, "hash": {"true"}}, &st); err != nil {
		return "", err
	}
	if err := kuboJSON(ctx, "pin/add", arg(st.Hash), nil); err != nil {
		return "", err
	}
	var keys struct{ Keys []struct{ Name, Id string } }
	if err := kuboJSON(ctx, "key/list", nil, &keys); err != nil {
		return "", err
	}
	found := false
	for _, k := range keys.Keys {
		found = found || k.Name == app
	}
	if !found {
		if err := kuboJSON(ctx, "key/gen", url.Values{"arg": {app}, "type": {"ed25519"}, "ipns-base": {"base36"}}, nil); err != nil {
			return "", err
		}
	}
	var pub struct{ Name, Value string }
	err := kuboJSON(ctx, "name/publish", url.Values{
		"arg": {"/ipfs/" + st.Hash}, "key": {app}, "ipns-base": {"base36"}, "allow-offline": {"true"},
	}, &pub)
	log.Printf("%s:%s → %s ; dir %s published as /ipns/%s", app, tag, root, st.Hash, pub.Name)
	return pub.Name, err
}

// resolveTag maps /ipns/<name>/<tag> to an image root CID. A name that points straight at an
// image root (not a tag directory) is accepted for the "latest" tag.
func resolveTag(ctx context.Context, name, tag string) (string, error) {
	var r struct{ Path string }
	err := kuboJSON(ctx, "resolve", url.Values{"arg": {"/ipns/" + name + "/" + tag}, "recursive": {"true"}}, &r)
	if err != nil && tag == "latest" {
		err = kuboJSON(ctx, "resolve", url.Values{"arg": {"/ipns/" + name}, "recursive": {"true"}}, &r)
	}
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(r.Path, "/ipfs/"), nil
}

// listTags returns every image root in the name's tag directory (used when a pull starts by digest).
func listTags(ctx context.Context, name string) []string {
	var ls struct {
		Objects []struct{ Links []struct{ Name, Hash string } }
	}
	if err := kuboJSON(ctx, "ls", arg("/ipns/"+name), &ls); err != nil || len(ls.Objects) == 0 {
		return nil
	}
	var roots []string
	for _, l := range ls.Objects[0].Links {
		roots = append(roots, l.Hash)
	}
	return roots
}

// ---------------------------------------------------------------- registry front door

var (
	ipfsPath = regexp.MustCompile(`^/v2/ipfs/([a-z0-9]+)/(manifests|blobs)/(.+)$`)
	ipnsPath = regexp.MustCompile(`^/v2/ipns/([a-z0-9][a-z0-9.-]*)/(manifests|blobs)/(.+)$`)
)

// Docker fetches manifests (by tag) before blobs (by digest only, no tag), so remember which roots
// a name resolved to recently and look blobs up under those.
type rootMemory struct {
	mu    sync.Mutex
	roots map[string][]string // name → roots, most recent first
	hit   map[string]string   // name@digest → root
}

var mem = rootMemory{roots: map[string][]string{}, hit: map[string]string{}}

func (m *rootMemory) remember(name, root string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rs := []string{root}
	for _, r := range m.roots[name] {
		if r != root && len(rs) < 16 {
			rs = append(rs, r)
		}
	}
	m.roots[name] = rs
}

func (m *rootMemory) candidates(name, digest string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.hit[name+"@"+digest]; ok {
		return []string{r}
	}
	return append([]string(nil), m.roots[name]...)
}

func (m *rootMemory) found(name, digest, root string) {
	m.mu.Lock()
	m.hit[name+"@"+digest] = root
	m.mu.Unlock()
}

func serve() error {
	up, err := url.Parse(upstream)
	if err != nil {
		return err
	}
	proxy := httputil.NewSingleHostReverseProxy(up)
	probe := &http.Client{Timeout: 5 * time.Minute}

	// exists asks nerdctl's registry whether path is servable under root.
	exists := func(ctx context.Context, path string) bool {
		req, _ := http.NewRequestWithContext(ctx, "HEAD", upstream+path, nil)
		resp, err := probe.Do(req)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}
	forward := func(w http.ResponseWriter, r *http.Request, path string) {
		r2 := r.Clone(r.Context())
		r2.URL.Path, r2.URL.RawPath = path, ""
		proxy.ServeHTTP(w, r2)
	}

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		if r.Method != "GET" && r.Method != "HEAD" {
			ociError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "read-only registry")
			return
		}
		if r.URL.Path == "/v2/" || r.URL.Path == "/v2" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if m := ipfsPath.FindStringSubmatch(r.URL.Path); m != nil {
			if m[2] == "manifests" && m[3] == "latest" {
				autoPinOnce(m[1])
			}
			forward(w, r, r.URL.Path)
			return
		}
		m := ipnsPath.FindStringSubmatch(r.URL.Path)
		if m == nil {
			ociError(w, http.StatusNotFound, "NAME_UNKNOWN", "use <host>/ipfs/<cid> or <host>/ipns/<name>:<tag>")
			return
		}
		name, kind, ref := m[1], m[2], m[3]
		ctx := r.Context()

		if kind == "manifests" && !strings.Contains(ref, ":") { // a tag
			root, err := resolveTag(ctx, name, ref)
			if err != nil {
				log.Printf("resolve %s:%s: %v", name, ref, err)
				ociError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", err.Error())
				return
			}
			log.Printf("%s:%s → %s", name, ref, root)
			mem.remember(name, root)
			autoPinOnce(root)
			forward(w, r, "/v2/ipfs/"+root+"/manifests/latest")
			return
		}

		// by digest (child manifest, config or layer): find which known root contains it
		cands := mem.candidates(name, ref)
		if len(cands) == 0 {
			cands = listTags(ctx, name)
		}
		for _, root := range cands {
			p := "/v2/ipfs/" + root + "/" + kind + "/" + ref
			if exists(ctx, p) {
				mem.found(name, ref, root)
				forward(w, r, p)
				return
			}
		}
		code := "BLOB_UNKNOWN"
		if kind == "manifests" {
			code = "MANIFEST_UNKNOWN"
		}
		ociError(w, http.StatusNotFound, code, ref+" not found under /ipns/"+name)
	})

	log.Printf("listening on %s (kubo %s, nerdctl registry %s, auto-pin %v, tls %v)", listen, kuboAPI, upstream, autoPin, tlsDir != "")
	if tlsDir == "" {
		return http.ListenAndServe(listen, h)
	}
	cert, err := ensureTLS()
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: listen, Handler: h, TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}}}
	return srv.ListenAndServeTLS("", "")
}

func ociError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"code": code, "message": msg}}})
}

// ---------------------------------------------------------------- TLS
//
// Docker only speaks plain HTTP to registries listed as insecure in daemon.json (a daemon reload).
// Instead we mint a private CA once and drop it in <certs.d>/<host:port>/ca.crt, which Docker and
// Podman read on every pull — no daemon change.

func ensureTLS() (tls.Certificate, error) {
	if err := os.MkdirAll(tlsDir, 0o700); err != nil {
		return tls.Certificate{}, err
	}
	caCrt, caKey := filepath.Join(tlsDir, "ca.crt"), filepath.Join(tlsDir, "ca.key")
	if _, err := os.Stat(caCrt); err != nil {
		if err := writeCert(caCrt, caKey, nil, nil, &x509.Certificate{
			Subject:               pkix.Name{CommonName: "ipcr local CA"},
			IsCA:                  true,
			BasicConstraintsValid: true,
			KeyUsage:              x509.KeyUsageCertSign,
			NotAfter:              time.Now().AddDate(20, 0, 0),
		}); err != nil {
			return tls.Certificate{}, err
		}
		log.Printf("created CA %s", caCrt)
	}
	ca, err := tls.LoadX509KeyPair(caCrt, caKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	ca.Leaf, _ = x509.ParseCertificate(ca.Certificate[0])

	host := strings.Split(hostName, ":")[0]
	crt, key := filepath.Join(tlsDir, "server.crt"), filepath.Join(tlsDir, "server.key")
	leaf, err := tls.LoadX509KeyPair(crt, key)
	if err == nil {
		leaf.Leaf, _ = x509.ParseCertificate(leaf.Certificate[0])
	}
	if err != nil || leaf.Leaf.VerifyHostname(host) != nil || time.Until(leaf.Leaf.NotAfter) < 30*24*time.Hour {
		if err := writeCert(crt, key, ca.Leaf, ca.PrivateKey, &x509.Certificate{
			Subject:     pkix.Name{CommonName: host},
			DNSNames:    []string{host, "localhost"},
			IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
			KeyUsage:    x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			NotAfter:    time.Now().AddDate(2, 0, 0),
		}); err != nil {
			return tls.Certificate{}, err
		}
		if leaf, err = tls.LoadX509KeyPair(crt, key); err != nil {
			return tls.Certificate{}, err
		}
	}
	if caExport != "" {
		pemCA, err := os.ReadFile(caCrt)
		if err == nil {
			err = os.MkdirAll(filepath.Dir(caExport), 0o755)
		}
		if err == nil {
			err = os.WriteFile(caExport, pemCA, 0o644)
		}
		if err != nil {
			// Not fatal: the registry still serves, Docker just won't trust it until the CA lands.
			log.Printf("export CA to %s: %v", caExport, err)
		} else {
			log.Printf("exported CA to %s", caExport)
		}
	}
	return leaf, nil
}

func writeCert(crtPath, keyPath string, parent *x509.Certificate, parentKey any, tmpl *x509.Certificate) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tmpl.SerialNumber = serial
	tmpl.NotBefore = time.Now().Add(-time.Hour)
	if parent == nil {
		parent, parentKey = tmpl, key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
	if err != nil {
		return err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		return err
	}
	return os.WriteFile(crtPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

// health is a dependency-free probe for container healthchecks. The gateway's certificate is
// for ipcr.localhost, not 127.0.0.1's caller, so verification is skipped: this only asks
// "is the listener answering", never "is it trusted".
func health(u string) error {
	c := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	resp, err := c.Get(u)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", u, resp.Status)
	}
	return nil
}
