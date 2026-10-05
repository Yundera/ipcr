// ipcrd — IPCR gateway, a thin layer over `nerdctl ipfs registry serve`.
//
//	serve                     registry front door: /v2/ipfs/<cid> passthrough + /v2/ipns/<name>[/<path>]:<tag>
//	pin <rootCID>...          pin an nerdctl IPFS image *and every blob it references*
//	tag <app> <tag> <rootCID> add a tag to the app's tag directory (MFS) and publish it under IPNS key <app>
//	resolve <name> [tag]      print the root CID a name/tag resolves to
//	import <ref> [app:tag]    copy an image from any registry into IPFS (no containerd), optionally tag it
//	key decrypt <backup>      print a publisher key backup's key, for `ipfs key import` (keybackup.go)
//	health [url]              exit 0 if url (default: this gateway's /v2/) answers 200 — for healthchecks
//
// nerdctl stores each blob as its own IPFS object and links them only via `urls: ["ipfs://…"]`
// inside the JSON, so pinning the root CID alone leaves the layers collectable. `pin` walks them.
//
// IPNS layout: /ipns/<name> → UnixFS directory whose entries are tags, each entry being an image
// root CID (as printed by `nerdctl push ipfs://`). <name> is an IPNS key (k51…) or a DNSLink domain
// (which includes ENS: Kubo resolves `.eth` names). A publisher with several images puts one tag
// directory per image under its name: /ipns/<name>/<image>/<tag>, pulled as
// ipcr.localhost:4767/ipns/<name>/<image>:<tag>. The path may have any number of segments.
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
	case "import":
		if len(args) < 1 || len(args) > 2 {
			usage()
		}
		var root string
		if root, err = importImage(ctx, args[0]); err != nil {
			break
		}
		fmt.Println(root)
		if len(args) == 2 {
			app, tag, _ := strings.Cut(args[1], ":")
			if tag == "" {
				tag = "latest"
			}
			var name string
			if name, err = tagImage(ctx, app, tag, root); err == nil {
				fmt.Println(name)
			}
		}
	case "key":
		// key decrypt <backup.ipcrkey.json>: the key in Kubo's export format, on stdout, for
		// `ipfs key import <name> <file>`. The passphrase: IPCR_KEY_PASSPHRASE, else stdin's first line.
		if len(args) != 2 || args[0] != "decrypt" {
			usage()
		}
		err = decryptBackup(args[1])
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
	fmt.Fprintln(os.Stderr, "usage: ipcrd serve | pin <cid>... | tag <app> <tag> <cid> | resolve <name> [tag] | import <ref> [app:tag] | key decrypt <backup> | health [url]")
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
		announce(ctx, root) // this node now holds it: say so now, not at the next reprovide
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

// tagImage adds <tag> → root to the app's tag directory and publishes that directory under the
// IPNS key named <app> (one key per image).
func tagImage(ctx context.Context, app, tag, root string) (string, error) {
	return updateTree(ctx, app, "/images/"+app, 0, func(dir string) error {
		return setTag(ctx, dir, tag, root)
	})
}

// publishImage adds <tag> → root under <repo> in the tree published by the IPNS key <key> — one
// key for many images: /ipns/<key>/<repo>/<tag>. <repo> may contain "/" (nested directories).
// It returns the pull name, "<k51…>/<repo>".
func publishImage(ctx context.Context, key, repo, tag, root string) (string, error) {
	name, err := updateTree(ctx, key, publisherDir(key), 0, func(base string) error {
		return setTag(ctx, base+"/"+repo, tag, root)
	})
	if err != nil {
		return "", err
	}
	return name + "/" + repo, nil
}

// publishPaths sets <tag> → root under every one of paths in the publisher tree, in one update.
func publishPaths(ctx context.Context, key string, paths []string, tag, root string) (string, error) {
	return updateTree(ctx, key, publisherDir(key), 0, func(base string) error {
		for _, p := range paths {
			if err := setTag(ctx, base+"/"+p, tag, root); err != nil {
				return err
			}
		}
		return nil
	})
}

func publisherDir(key string) string { return "/publishers/" + key }

// One published tree at a time: two writers would each publish a tree missing the other's change.
var publishMu sync.Mutex

// updateTree applies change to the MFS directory dir, then publishes dir's new CID under the IPNS
// key <key> (created on first use) with sequence seq (0: Kubo's next). The new root is pinned and
// the previous one unpinned, so superseded trees do not pile up as pins. The images themselves
// keep their own pins. Returns the key's IPNS name.
func updateTree(ctx context.Context, key, dir string, seq uint64, change func(dir string) error) (string, error) {
	publishMu.Lock()
	defer publishMu.Unlock()
	if err := kuboJSON(ctx, "files/mkdir", url.Values{"arg": {dir}, "parents": {"true"}}, nil); err != nil {
		return "", err
	}
	old, _ := mfsHash(ctx, dir)
	if change != nil {
		if err := change(dir); err != nil {
			return "", err
		}
	}
	cur, err := mfsHash(ctx, dir)
	if err != nil {
		return "", err
	}
	if err := kuboJSON(ctx, "pin/add", arg(cur), nil); err != nil {
		return "", err
	}
	if err := ensureKey(ctx, key); err != nil {
		return "", err
	}
	name, err := publishRecord(ctx, key, cur, seq)
	if err != nil {
		return "", err
	}
	if old != "" && old != cur {
		// Not recursive pins of the images: only this directory's own pin goes.
		if err := kuboJSON(ctx, "pin/rm", arg(old), nil); err != nil {
			log.Printf("unpin previous tree %s: %v", old, err)
		}
	}
	return name, nil
}

func mfsHash(ctx context.Context, dir string) (string, error) {
	var st struct{ Hash string }
	err := kuboJSON(ctx, "files/stat", url.Values{"arg": {dir}, "hash": {"true"}}, &st)
	return st.Hash, err
}

// setTag sets <dir>/<tag> → root in Kubo's MFS.
func setTag(ctx context.Context, dir, tag, root string) error {
	if err := kuboJSON(ctx, "files/mkdir", url.Values{"arg": {dir}, "parents": {"true"}}, nil); err != nil {
		return err
	}
	_ = kuboJSON(ctx, "files/rm", url.Values{"arg": {dir + "/" + tag}, "force": {"true"}}, nil)
	return kuboJSON(ctx, "files/cp", arg("/ipfs/"+root, dir+"/"+tag), nil)
}

// ensureKey creates the IPNS key <name> if this node does not have it.
func ensureKey(ctx context.Context, name string) error {
	id, err := keyID(ctx, name)
	if err != nil || id != "" {
		return err
	}
	return kuboJSON(ctx, "key/gen", url.Values{"arg": {name}, "type": {"ed25519"}, "ipns-base": {"base36"}}, nil)
}

type kuboKey struct{ Name, Id string }

// listKeys returns this node's keys, IPNS names in base36 (k51…).
func listKeys(ctx context.Context) ([]kuboKey, error) {
	var keys struct{ Keys []kuboKey }
	err := kuboJSON(ctx, "key/list", url.Values{"ipns-base": {"base36"}}, &keys)
	return keys.Keys, err
}

// keyID returns the IPNS name (k51…) of the key <name>, or "" when there is none.
func keyID(ctx context.Context, name string) (string, error) {
	keys, err := listKeys(ctx)
	if err != nil {
		return "", err
	}
	for _, k := range keys {
		if k.Name == name {
			return k.Id, nil
		}
	}
	return "", nil
}

// Record validity and caching. Kubo's default lifetime is 48h: a node offline for a weekend would
// take every name with it. Republishing (every 4h, by Kubo) keeps the longer expiry. TTL is how
// long resolvers may cache the record, so how quickly a moved tag is seen.
var (
	ipnsLifetime = env("IPNS_LIFETIME", "48h")
	ipnsTTL      = env("IPNS_TTL", "5m")
)

// publishRecord points the IPNS key <key> at /ipfs/<dir>. seq 0 lets Kubo pick its next sequence
// number, which it only knows from its own datastore: after a key is restored on another node, the
// caller must pass one above the network's, or the network keeps the old record.
func publishRecord(ctx context.Context, key, dir string, seq uint64) (string, error) {
	v := url.Values{
		"arg": {"/ipfs/" + dir}, "key": {key}, "ipns-base": {"base36"}, "allow-offline": {"true"},
		"lifetime": {ipnsLifetime}, "ttl": {ipnsTTL},
	}
	if seq > 0 {
		v.Set("sequence", fmt.Sprint(seq))
	}
	var pub struct{ Name, Value string }
	if err := kuboJSON(ctx, "name/publish", v, &pub); err != nil {
		return "", err
	}
	log.Printf("key %s: dir %s published as /ipns/%s", key, dir, pub.Name)
	return pub.Name, nil
}

// resolveTag maps /ipns/<name>/<tag> to an image root CID. <name> may carry a path
// (aptero.eth/myapp). A name that points straight at an image root (not a tag directory) is
// accepted for the "latest" tag.
func resolveTag(ctx context.Context, name, tag string) (string, error) {
	base, err := namePath(ctx, name)
	if err != nil {
		return "", err
	}
	var r struct{ Path string }
	err = kuboJSON(ctx, "resolve", url.Values{"arg": {base + "/" + tag}, "recursive": {"true"}}, &r)
	if err != nil && tag == "latest" {
		err = kuboJSON(ctx, "resolve", url.Values{"arg": {base}, "recursive": {"true"}}, &r)
	}
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(r.Path, "/ipfs/"), nil
}

// namePath is where a name's tag directory is: /ipns/<name> in general, but for a name this node
// publishes, /ipfs/<its current tree>/<path>, read from the node's own record without Kubo's cache
// (see ownName). Only `name/resolve` honours nocache; the generic `resolve` ignores it.
func namePath(ctx context.Context, name string) (string, error) {
	name, own := ownName(ctx, name)
	if !own {
		return "/ipns/" + name, nil
	}
	key, rest, _ := strings.Cut(name, "/")
	var r struct{ Path string }
	if err := kuboJSON(ctx, "name/resolve", url.Values{"arg": {key}, "nocache": {"true"}}, &r); err != nil {
		return "", err
	}
	if rest != "" {
		return r.Path + "/" + rest, nil
	}
	return r.Path, nil
}

// ownName rewrites a name published by this node, directly (k51…/app) or through a DNSLink or ENS
// name pointing at one of its keys (example.eth/app), to the key itself, and says so. Such names
// are resolved without Kubo's cache: Kubo does not always refresh its cache when it publishes (a
// tree published again after a change in between kept resolving to the in-between tree for the
// record's TTL), and the node's own record is local, so skipping the cache costs nothing.
func ownName(ctx context.Context, name string) (string, bool) {
	first, rest, _ := strings.Cut(name, "/")
	ids := ownKeyIDs(ctx)
	if ids[first] {
		return name, true
	}
	if !strings.Contains(first, ".") {
		return name, false
	}
	var r struct{ Path string }
	if kuboJSON(ctx, "resolve", url.Values{"arg": {"/ipns/" + first}, "recursive": {"false"}}, &r) != nil {
		return name, false
	}
	k := strings.TrimPrefix(strings.TrimSuffix(r.Path, "/"), "/ipns/")
	if !ids[k] {
		return name, false
	}
	if rest != "" {
		return k + "/" + rest, true
	}
	return k, true
}

var ownKeys struct {
	sync.Mutex
	ids map[string]bool
	at  time.Time
}

// ownKeyIDs: this node's IPNS names (k51…), refreshed every 30 s.
func ownKeyIDs(ctx context.Context) map[string]bool {
	ownKeys.Lock()
	defer ownKeys.Unlock()
	if ownKeys.ids != nil && time.Since(ownKeys.at) < 30*time.Second {
		return ownKeys.ids
	}
	keys, err := listKeys(ctx)
	if err != nil {
		return ownKeys.ids
	}
	ids := map[string]bool{}
	for _, k := range keys {
		if k.Name != "self" {
			ids[k.Id] = true
		}
	}
	ownKeys.ids, ownKeys.at = ids, time.Now()
	return ids
}

// listTags returns every image root in the name's tag directory (used when a pull starts by digest).
func listTags(ctx context.Context, name string) []string {
	base, err := namePath(ctx, name)
	if err != nil {
		return nil
	}
	var ls struct {
		Objects []struct{ Links []struct{ Name, Hash string } }
	}
	if err := kuboJSON(ctx, "ls", arg(base), &ls); err != nil || len(ls.Objects) == 0 {
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
	// <name> then any number of path segments in Docker's path-component grammar. The reference
	// (tag or digest) never holds a "/", which keeps the split unambiguous.
	ipnsPath = regexp.MustCompile(`^/v2/ipns/([a-z0-9][a-z0-9.-]*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*)/(manifests|blobs)/([^/]+)$`)
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
			if public != nil {
				if ok, why := public.allows("ipfs", m[1]); !ok {
					ociError(w, http.StatusNotFound, "NAME_UNKNOWN", why)
					return
				}
			}
			if m[2] == "manifests" && m[3] == "latest" {
				autoPinOnce(m[1])
			}
			forward(w, r, r.URL.Path)
			return
		}
		m := ipnsPath.FindStringSubmatch(r.URL.Path)
		if m == nil {
			ociError(w, http.StatusNotFound, "NAME_UNKNOWN", "use <host>/ipfs/<cid> or <host>/ipns/<name>[/<image>]:<tag>")
			return
		}
		name, kind, ref := m[1], m[2], m[3]
		if public != nil {
			if ok, why := public.allows("ipns", name); !ok {
				ociError(w, http.StatusNotFound, "NAME_UNKNOWN", why)
				return
			}
		}
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

	if err := ensureImportAuth(); err != nil {
		return fmt.Errorf("IMPORT_AUTH_FILE: %w", err)
	}
	if w := os.Getenv("IMPORT_WATCH"); w != "" {
		every, err := time.ParseDuration(env("IMPORT_INTERVAL", "60s"))
		if err != nil {
			return fmt.Errorf("IMPORT_INTERVAL: %w", err)
		}
		if theWatcher, err = newWatcher(w, every, env("STATE_DIR", "/data/state")); err != nil {
			return err
		}
		go theWatcher.run(context.Background())
	}
	if a := os.Getenv("ADMIN_LISTEN"); a != "" {
		if err := serveAdmin(a, env("STATE_DIR", "/data/state")); err != nil {
			return err
		}
	}

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
