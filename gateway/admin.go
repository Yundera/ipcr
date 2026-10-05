package main

// The admin API: what a publisher's operator needs beyond the registry. Served on ADMIN_LISTEN,
// plain HTTP, every request with `Authorization: Bearer <STATE_DIR/admin-token>`. It is meant for
// one client on a private network (IPCR Forge's service, which puts a login in front of it), not for
// people: it can unpublish images and replace the publisher key.
//
//	GET  /admin/status            key, node, storage, IPNS record (here and on the network), name check
//	GET  /admin/providers?cid=    is this node listed as a provider of cid (delegated routing)
//	POST /admin/announce {cid}    announce cid now
//	GET  /admin/peers             this node's addresses as the network knows them
//	GET  /admin/images            what is published, what the source registry holds, the allowlist
//	POST /admin/unpublish {repo,tag}
//	POST /admin/retag {repo,tag,from}
//	POST /admin/reimport {repo,tag}
//	GET  /admin/config            config.json; PATCH it with {name?, allow?}
//	POST /admin/republish         publish the current tree again (new sequence, full lifetime)
//	POST /admin/key/import {backup, passphrase, sequence?}
//
// Everything but status and config needs the watcher in publisher mode (IMPORT_WATCH and
// IMPORT_PUBLISHER): one key, one tree, one published.json.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type admin struct {
	stateDir string
	token    []byte

	mu     sync.Mutex
	status *adminStatus // cached: the network checks take seconds
}

func serveAdmin(addr, stateDir string) error {
	tok, err := adminToken(stateDir)
	if err != nil {
		return err
	}
	a := &admin{stateDir: stateDir, token: tok}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/status", a.handleStatus)
	mux.HandleFunc("GET /admin/providers", a.handleProviders)
	mux.HandleFunc("POST /admin/announce", a.handleAnnounce)
	mux.HandleFunc("GET /admin/peers", a.handlePeers)
	mux.HandleFunc("GET /admin/images", a.handleImages)
	mux.HandleFunc("POST /admin/unpublish", a.handleUnpublish)
	mux.HandleFunc("POST /admin/retag", a.handleRetag)
	mux.HandleFunc("POST /admin/reimport", a.handleReimport)
	mux.HandleFunc("GET /admin/config", a.handleConfig)
	mux.HandleFunc("PATCH /admin/config", a.handleConfig)
	mux.HandleFunc("POST /admin/republish", a.handleRepublish)
	mux.HandleFunc("POST /admin/key/import", a.handleKeyImport)
	go func() { log.Fatal(http.ListenAndServe(addr, a.auth(mux))) }()
	// The name is shown in pull lines only while it checks out: re-check it now and then.
	go func() {
		for {
			if w := theWatcher; w != nil && w.publisher != "" {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				a.checkName(ctx, w)
				cancel()
			}
			time.Sleep(30 * time.Minute)
		}
	}()
	log.Printf("admin API on %s", addr)
	return nil
}

// adminToken reads STATE_DIR/admin-token, creating it on first start. World-readable on purpose:
// this process is root without capabilities, so it cannot hand a file to another uid any other
// way. The folder is the app's own state, read by the client through a read-only mount.
func adminToken(stateDir string) ([]byte, error) {
	p := filepath.Join(stateDir, "admin-token")
	if b, err := os.ReadFile(p); err == nil && len(bytes.TrimSpace(b)) >= 32 {
		return bytes.TrimSpace(b), nil
	}
	b := make([]byte, 32)
	rand.Read(b)
	tok := []byte(hex.EncodeToString(b))
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(p+".tmp", tok, 0o644); err != nil {
		return nil, err
	}
	return tok, os.Rename(p+".tmp", p)
}

func (a *admin) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), a.token) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func decode(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

var errNoPublisher = errors.New("needs IMPORT_WATCH and IMPORT_PUBLISHER (one publisher key)")

// publisherWatcher returns the watcher, when it publishes under one key.
func publisherWatcher() (*watcher, error) {
	if w := theWatcher; w != nil && w.publisher != "" {
		return w, nil
	}
	return nil, errNoPublisher
}

// ---------------------------------------------------------------- Kubo helpers

// kuboUpload is a Kubo RPC call whose argument is a file (name/inspect's record, key/import's key).
func kuboUpload(ctx context.Context, cmd string, args url.Values, data []byte, out any) error {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "file")
	fw.Write(data)
	mw.Close()
	req, err := http.NewRequestWithContext(ctx, "POST", kuboAPI+"/api/v0/"+cmd+"?"+args.Encode(), &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := kuboClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct{ Message string }
		if json.Unmarshal(b, &e) != nil || e.Message == "" {
			e.Message = strings.TrimSpace(string(b))
		}
		return fmt.Errorf("kubo %s: %s", cmd, e.Message)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

// record is what the admin page shows of an IPNS record.
type record struct {
	Value    string `json:"value"`
	Sequence uint64 `json:"sequence"`
	Validity string `json:"validity"`
	TTL      string `json:"ttl,omitempty"`
	Valid    bool   `json:"valid"`
	Reason   string `json:"reason,omitempty"`
}

// inspect decodes and verifies a raw IPNS record against the name it should belong to.
func inspect(ctx context.Context, raw []byte, name string) (*record, error) {
	var out struct {
		Entry struct {
			Value    string
			Validity *time.Time
			Sequence *uint64
			TTL      *time.Duration
		}
		Validation *struct {
			Valid  bool
			Reason string
		}
	}
	if err := kuboUpload(ctx, "name/inspect", url.Values{"verify": {name}}, raw, &out); err != nil {
		return nil, err
	}
	r := &record{Value: out.Entry.Value}
	if out.Entry.Sequence != nil {
		r.Sequence = *out.Entry.Sequence
	}
	if out.Entry.Validity != nil {
		r.Validity = out.Entry.Validity.UTC().Format(time.RFC3339)
	}
	if out.Entry.TTL != nil {
		r.TTL = out.Entry.TTL.String()
	}
	if out.Validation != nil {
		r.Valid, r.Reason = out.Validation.Valid, out.Validation.Reason
	}
	return r, nil
}

// localRecord is the best record Kubo finds for name (its own datastore first).
func localRecord(ctx context.Context, name string) (*record, error) {
	body, err := kubo(ctx, "name/get", arg(name))
	if err != nil {
		return nil, err
	}
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, 64<<10))
	if err != nil {
		return nil, err
	}
	return inspect(ctx, raw, name)
}

// ---------------------------------------------------------------- delegated routing

// The public view of the network: a delegated routing endpoint (IPFS's HTTP routing API) that also
// queries the DHT. It answers "can someone else find this", which this node cannot ask itself.
var (
	delegated       = strings.TrimRight(env("DELEGATED_ROUTING", "https://delegated-ipfs.dev"), "/")
	delegatedClient = &http.Client{Timeout: 30 * time.Second}
)

var errNotFound = errors.New("not found")

func delegatedGet(ctx context.Context, p, accept string) ([]byte, error) {
	b, _, err := delegatedFetch(ctx, p, accept)
	return b, err
}

// delegatedFetch also returns the answer's content type: delegated-ipfs.dev answers a lookup it
// cannot satisfy with 200 text/plain "delegate error: routing: not found", not the spec's 404.
func delegatedFetch(ctx context.Context, p, accept string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", delegated+p, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", accept)
	resp, err := delegatedClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	ct := resp.Header.Get("Content-Type")
	if resp.StatusCode == http.StatusNotFound || (strings.HasPrefix(ct, "text/plain") && strings.Contains(string(b), "not found")) {
		return nil, ct, errNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, ct, fmt.Errorf("%s: %s", delegated, resp.Status)
	}
	return b, ct, nil
}

// networkRecord is the record the network serves for name: nil, nil when there is none.
func networkRecord(ctx context.Context, name string) (*record, error) {
	const recordType = "application/vnd.ipfs.ipns-record"
	raw, ct, err := delegatedFetch(ctx, "/routing/v1/ipns/"+name, recordType)
	if errors.Is(err, errNotFound) || (err == nil && len(raw) == 0) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(ct, recordType) {
		return nil, fmt.Errorf("%s answered %s: %.100s", delegated, ct, raw)
	}
	return inspect(ctx, raw, name)
}

type routingPeer struct {
	ID    string
	Addrs []string
}

func delegatedPeers(ctx context.Context, p string) ([]routingPeer, error) {
	b, err := delegatedGet(ctx, p, "application/json")
	if errors.Is(err, errNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out struct {
		Providers []routingPeer
		Peers     []routingPeer
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return append(out.Providers, out.Peers...), nil
}

func selfID(ctx context.Context) (string, []string, error) {
	var id struct {
		ID        string
		Addresses []string
	}
	err := kuboJSON(ctx, "id", nil, &id)
	return id.ID, id.Addresses, err
}

// ---------------------------------------------------------------- status

type adminStatus struct {
	CheckedAt string `json:"checkedAt"`
	Key       struct {
		Name string `json:"name"`
		ID   string `json:"id"`
	} `json:"key"`
	Node struct {
		ID        string   `json:"id"`
		Addresses []string `json:"addresses"`
		Peers     int      `json:"peers"`
	} `json:"node"`
	Storage struct {
		RepoSize   uint64 `json:"repoSize"`
		StorageMax uint64 `json:"storageMax"`
		Pins       int    `json:"pins"`
	} `json:"storage"`
	Record struct {
		Lifetime     string  `json:"lifetime"`
		TTL          string  `json:"ttl"`
		Tree         string  `json:"tree"`
		Local        *record `json:"local"`
		LocalError   string  `json:"localError,omitempty"`
		Network      *record `json:"network"`
		NetworkError string  `json:"networkError,omitempty"`
	} `json:"record"`
	Name nameCheck `json:"name"`
}

type nameCheck struct {
	Name      string `json:"name,omitempty"`
	OK        bool   `json:"ok"`
	Resolves  string `json:"resolves,omitempty"`
	Error     string `json:"error,omitempty"`
	CheckedAt string `json:"checkedAt,omitempty"`
}

func (a *admin) handleStatus(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	cached := a.status
	a.mu.Unlock()
	if cached != nil && r.URL.Query().Get("refresh") == "" {
		if t, err := time.Parse(time.RFC3339, cached.CheckedAt); err == nil && time.Since(t) < 5*time.Minute {
			reply(w, cached)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	s := a.collect(ctx)
	a.mu.Lock()
	a.status = s
	a.mu.Unlock()
	reply(w, s)
}

func (a *admin) forget() {
	a.mu.Lock()
	a.status = nil
	a.mu.Unlock()
}

func (a *admin) collect(ctx context.Context) *adminStatus {
	s := &adminStatus{CheckedAt: time.Now().UTC().Format(time.RFC3339)}
	s.Record.Lifetime, s.Record.TTL = ipnsLifetime, ipnsTTL
	if id, addrs, err := selfID(ctx); err == nil {
		s.Node.ID, s.Node.Addresses = id, addrs
	}
	var peers struct{ Peers []json.RawMessage }
	if kuboJSON(ctx, "swarm/peers", nil, &peers) == nil {
		s.Node.Peers = len(peers.Peers)
	}
	var st struct{ RepoSize, StorageMax uint64 }
	if kuboJSON(ctx, "repo/stat", url.Values{"size-only": {"true"}}, &st) == nil {
		s.Storage.RepoSize, s.Storage.StorageMax = st.RepoSize, st.StorageMax
	}
	var pins struct{ Keys map[string]json.RawMessage }
	if kuboJSON(ctx, "pin/ls", url.Values{"type": {"recursive"}, "quiet": {"true"}}, &pins) == nil {
		s.Storage.Pins = len(pins.Keys)
	}
	w, err := publisherWatcher()
	if err != nil {
		return s
	}
	s.Key.Name = w.publisher
	s.Key.ID, _ = keyID(ctx, w.publisher)
	if s.Key.ID == "" {
		return s
	}
	if h, err := mfsHash(ctx, publisherDir(w.publisher)); err == nil {
		s.Record.Tree = "/ipfs/" + h
	}
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		if rec, err := localRecord(ctx, s.Key.ID); err != nil {
			s.Record.LocalError = err.Error()
		} else {
			s.Record.Local = rec
		}
	}()
	go func() {
		defer wg.Done()
		if rec, err := networkRecord(ctx, s.Key.ID); err != nil {
			s.Record.NetworkError = err.Error()
		} else if rec == nil {
			s.Record.NetworkError = "no record found on the network"
		} else {
			s.Record.Network = rec
		}
	}()
	go func() {
		defer wg.Done()
		s.Name = a.checkName(ctx, w)
	}()
	wg.Wait()
	return s
}

// checkName resolves the configured name and records in published.json whether it points at this
// publisher: pull lines use the name only while it does.
func (a *admin) checkName(ctx context.Context, w *watcher) nameCheck {
	c := nameCheck{Name: loadConfig(a.stateDir).Name, CheckedAt: time.Now().UTC().Format(time.RFC3339)}
	id, err := keyID(ctx, w.publisher)
	if c.Name == "" || err != nil || id == "" {
		w.setName("", "")
		return c
	}
	var res struct{ Path string }
	err = kuboJSON(ctx, "resolve", url.Values{"arg": {"/ipns/" + c.Name}, "recursive": {"false"}, "nocache": {"true"}}, &res)
	switch {
	case err != nil:
		c.Error = err.Error()
	case strings.TrimSuffix(res.Path, "/") == "/ipns/"+id:
		c.OK, c.Resolves = true, res.Path
	default:
		c.Resolves = res.Path
		c.Error = "points elsewhere: set it to ipns://" + id
	}
	if c.OK {
		w.setName(c.Name, c.CheckedAt)
	} else if err == nil {
		// A failed lookup (resolver down) keeps the last good answer; a wrong target does not.
		w.setName("", "")
	}
	return c
}

func (w *watcher) setName(name, at string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pub.Name == name && (name == "" || w.pub.NameVerifiedAt == at) {
		return
	}
	w.pub.Name, w.pub.NameVerifiedAt = name, at
	w.save()
}

// ---------------------------------------------------------------- network checks

func (a *admin) handleProviders(w http.ResponseWriter, r *http.Request) {
	cid := r.URL.Query().Get("cid")
	if cid == "" {
		fail(w, http.StatusBadRequest, errors.New("cid?"))
		return
	}
	self, _, err := selfID(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	provs, err := delegatedPeers(r.Context(), "/routing/v1/providers/"+url.PathEscape(cid))
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	found := false
	for _, p := range provs {
		found = found || p.ID == self
	}
	reply(w, map[string]any{"cid": cid, "self": found, "providers": len(provs)})
}

func (a *admin) handleAnnounce(w http.ResponseWriter, r *http.Request) {
	var req struct{ CID string }
	if err := decode(r, &req); err != nil || req.CID == "" {
		fail(w, http.StatusBadRequest, errors.New("{cid}?"))
		return
	}
	if err := kuboJSON(r.Context(), "routing/provide", url.Values{"arg": {req.CID}}, nil); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	reply(w, map[string]any{"announced": req.CID})
}

func (a *admin) handlePeers(w http.ResponseWriter, r *http.Request) {
	self, local, err := selfID(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	peers, err := delegatedPeers(r.Context(), "/routing/v1/peers/"+self)
	out := map[string]any{"id": self, "local": local}
	if err != nil {
		out["error"] = err.Error()
	}
	var seen []string
	for _, p := range peers {
		if p.ID == self {
			seen = append(seen, p.Addrs...)
		}
	}
	out["network"] = seen
	reply(w, out)
}

// ---------------------------------------------------------------- images

type imageView struct {
	Repo    string       `json:"repo"`
	IPNS    string       `json:"ipns,omitempty"`
	Aliases []string     `json:"aliases,omitempty"`
	Allowed bool         `json:"allowed"`
	Tags    []tagView    `json:"tags"`
	Staging []stagingTag `json:"staging,omitempty"`
}

type tagView struct {
	Tag string `json:"tag"`
	publishedTag
}

type stagingTag struct {
	Tag    string `json:"tag"`
	Digest string `json:"digest,omitempty"`
	State  string `json:"state"` // published | unpublished | pending
}

func (a *admin) handleImages(w http.ResponseWriter, r *http.Request) {
	wt, err := publisherWatcher()
	if err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	cfg := loadConfig(a.stateDir)
	byRepo := map[string]*imageView{}
	view := func(repo string) *imageView {
		if v := byRepo[repo]; v != nil {
			return v
		}
		v := &imageView{Repo: repo, Allowed: cfg.allowed(repo), Tags: []tagView{}}
		byRepo[repo] = v
		return v
	}
	wt.mu.Lock()
	out := map[string]any{"publisher": wt.pub.Publisher, "name": wt.pub.Name, "registry": wt.pub.Registry, "public": cfg.publicOn()}
	for repo, pr := range wt.pub.Images {
		v := view(repo)
		v.IPNS, v.Aliases = pr.IPNS, pr.Aliases
		for tag, t := range pr.Tags {
			v.Tags = append(v.Tags, tagView{tag, t})
		}
	}
	state := make(map[string]string, len(wt.state))
	for k, d := range wt.state {
		state[k] = d
	}
	wt.mu.Unlock()

	// What the source registry holds now (the forge's staging registry: builds not yet imported,
	// or not allowed).
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	reg := newRegistry(wt.u.Scheme, wt.u.Host)
	if repos, err := reg.catalog(ctx); err != nil {
		out["stagingError"] = err.Error()
	} else {
		for _, repo := range repos {
			tags, err := reg.tags(ctx, repo)
			if err != nil {
				continue
			}
			v := view(repo)
			for _, tag := range tags {
				st := stagingTag{Tag: tag, State: "pending"}
				st.Digest, _ = reg.headDigest(ctx, repo, tag)
				switch prev := state[wt.stateKey(repo, tag)]; {
				case st.Digest != "" && prev == st.Digest:
					st.State = "published"
				case st.Digest != "" && prev == tombstone+st.Digest:
					st.State = "unpublished"
				}
				v.Staging = append(v.Staging, st)
			}
		}
	}
	list := make([]*imageView, 0, len(byRepo))
	for _, v := range byRepo {
		sort.Slice(v.Tags, func(i, j int) bool { return v.Tags[i].At > v.Tags[j].At })
		list = append(list, v)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Repo < list[j].Repo })
	out["images"] = list
	reply(w, out)
}

type repoTag struct {
	Repo string `json:"repo"`
	Tag  string `json:"tag"`
	From string `json:"from,omitempty"`
}

// The same grammar as an image path (see ipnsPath) and a Docker tag: nothing that escapes the tree.
var (
	repoRe = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)
	tagRe  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
)

func (rt repoTag) valid() error {
	if !repoRe.MatchString(rt.Repo) || !tagRe.MatchString(rt.Tag) || (rt.From != "" && !tagRe.MatchString(rt.From)) {
		return errors.New("bad repo or tag")
	}
	return nil
}

func (a *admin) handleUnpublish(w http.ResponseWriter, r *http.Request) {
	var req repoTag
	if err := decode(r, &req); err != nil || req.valid() != nil {
		fail(w, http.StatusBadRequest, errors.New("{repo, tag}?"))
		return
	}
	wt, err := publisherWatcher()
	if err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	wt.mu.Lock()
	var t publishedTag
	var paths []string
	ok := wt.pub.has(req.Repo, req.Tag)
	if ok {
		t = wt.pub.Images[req.Repo].Tags[req.Tag]
		paths = append([]string{req.Repo}, wt.pub.Images[req.Repo].Aliases...)
	}
	wt.mu.Unlock()
	if !ok {
		fail(w, http.StatusNotFound, errors.New("not published"))
		return
	}
	ctx := r.Context()
	base := publisherDir(wt.publisher)
	_, err = updateTree(ctx, wt.publisher, base, 0, func(base string) error {
		for _, p := range paths {
			if err := kuboJSON(ctx, "files/rm", url.Values{"arg": {base + "/" + p + "/" + req.Tag}, "force": {"true"}}, nil); err != nil {
				return err
			}
			// Remove the directories the tag leaves empty (repo, then its owner…).
			for dir := base + "/" + p; dir != base; dir = path.Dir(dir) {
				var ls struct{ Entries []json.RawMessage }
				if kuboJSON(ctx, "files/ls", arg(dir), &ls) != nil || len(ls.Entries) > 0 {
					break
				}
				kuboJSON(ctx, "files/rm", url.Values{"arg": {dir}, "recursive": {"true"}}, nil)
			}
		}
		return nil
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	wt.mu.Lock()
	wt.pub.remove(req.Repo, req.Tag)
	// The source registry may still hold the tag: do not import it again unless it moves.
	wt.state[wt.stateKey(req.Repo, req.Tag)] = tombstone + t.Digest
	wt.save()
	remaining := map[string]bool{}
	for _, pr := range wt.pub.Images {
		for _, x := range pr.Tags {
			remaining[x.CID] = true
		}
	}
	wt.mu.Unlock()
	a.forget()
	log.Printf("admin: unpublished %s:%s (/ipfs/%s)", req.Repo, req.Tag, t.CID)
	freed := 0
	if !remaining[t.CID] {
		freed = unpinUnused(ctx, t.CID, remaining)
	}
	reply(w, map[string]any{"unpublished": req.Repo + ":" + req.Tag, "cid": t.CID, "unpinned": freed})
}

// unpinUnused unpins the objects of image root that no remaining image uses (layers are often
// shared). The content stays wherever else it is pinned; here it becomes collectable.
func unpinUnused(ctx context.Context, root string, remaining map[string]bool) int {
	gone, err := imageCIDs(ctx, root)
	if err != nil {
		log.Printf("admin: unpin %s: %v", root, err)
		return 0
	}
	for r := range remaining {
		keep, err := imageCIDs(ctx, r)
		if err != nil {
			log.Printf("admin: unpin %s: cannot read %s, keeping everything: %v", root, r, err)
			return 0
		}
		for c := range keep {
			delete(gone, c)
		}
	}
	n := 0
	for c := range gone {
		if kuboJSON(ctx, "pin/rm", arg(c), nil) == nil {
			n++
		}
	}
	return n
}

// imageCIDs lists every object of an image (root, indexes, manifests, configs, layers): what
// pinImage pins.
func imageCIDs(ctx context.Context, root string) (map[string]bool, error) {
	raw, err := kuboCat(ctx, root)
	if err != nil {
		return nil, err
	}
	var d descriptor
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	set := map[string]bool{root: true}
	var walk func(d descriptor) error
	walk = func(d descriptor) error {
		c := cidOf(d)
		if c == "" || set[c] {
			return nil
		}
		set[c] = true
		if !hasChildren(d.MediaType) {
			return nil
		}
		raw, err := kuboCat(ctx, c)
		if err != nil {
			return err
		}
		var m struct {
			Manifests []descriptor `json:"manifests"`
			Config    *descriptor  `json:"config"`
			Layers    []descriptor `json:"layers"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		children := append(m.Manifests, m.Layers...)
		if m.Config != nil {
			children = append(children, *m.Config)
		}
		for _, ch := range children {
			if err := walk(ch); err != nil {
				return err
			}
		}
		return nil
	}
	return set, walk(d)
}

func (a *admin) handleRetag(w http.ResponseWriter, r *http.Request) {
	var req repoTag
	if err := decode(r, &req); err != nil || req.From == "" || req.valid() != nil {
		fail(w, http.StatusBadRequest, errors.New("{repo, tag, from}?"))
		return
	}
	wt, err := publisherWatcher()
	if err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	wt.mu.Lock()
	var src publishedTag
	var paths []string
	ok := wt.pub.has(req.Repo, req.From)
	if ok {
		src = wt.pub.Images[req.Repo].Tags[req.From]
		paths = append([]string{req.Repo}, wt.pub.Images[req.Repo].Aliases...)
	}
	wt.mu.Unlock()
	if !ok {
		fail(w, http.StatusNotFound, fmt.Errorf("%s:%s is not published", req.Repo, req.From))
		return
	}
	name, err := publishPaths(r.Context(), wt.publisher, paths, req.Tag, src.CID)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	name += "/" + req.Repo
	wt.mu.Lock()
	wt.pub.set(req.Repo, name, req.Tag, publishedTag{CID: src.CID, Digest: src.Digest, At: time.Now().UTC().Format(time.RFC3339)})
	wt.save()
	wt.mu.Unlock()
	a.forget()
	log.Printf("admin: %s:%s → %s (/ipfs/%s)", req.Repo, req.Tag, req.From, src.CID)
	reply(w, map[string]any{"tag": req.Repo + ":" + req.Tag, "cid": src.CID})
}

func (a *admin) handleReimport(w http.ResponseWriter, r *http.Request) {
	var req repoTag
	if err := decode(r, &req); err != nil || req.valid() != nil {
		fail(w, http.StatusBadRequest, errors.New("{repo, tag}?"))
		return
	}
	wt, err := publisherWatcher()
	if err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	wt.mu.Lock()
	delete(wt.state, wt.stateKey(req.Repo, req.Tag))
	wt.save()
	wt.mu.Unlock()
	wt.wake()
	reply(w, map[string]any{"queued": req.Repo + ":" + req.Tag,
		"note": "imported on the next pass if the source registry still holds the tag"})
}

// ---------------------------------------------------------------- config

var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

func (a *admin) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == "PATCH" {
		var req struct {
			Public *bool   `json:"public"`
			Name   *string `json:"name"`
			Allow  *struct {
				Required bool                `json:"required"`
				Repos    []string            `json:"repos"`
				Aliases  map[string][]string `json:"aliases"`
			} `json:"allow"`
		}
		if err := decode(r, &req); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		cfg := loadConfig(a.stateDir)
		nameChanged := false
		if req.Name != nil {
			n := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(*req.Name)), ".")
			if n != "" && !nameRe.MatchString(n) {
				fail(w, http.StatusBadRequest, errors.New("name: a domain (example.com) or an ENS name (example.eth)"))
				return
			}
			nameChanged = n != cfg.Name
			cfg.Name = n
		}
		if req.Allow != nil {
			seen := map[string]bool{}
			cfg.Allow.Required, cfg.Allow.Repos = req.Allow.Required, []string{}
			for _, repo := range req.Allow.Repos {
				repo = strings.ToLower(strings.TrimSpace(repo))
				if repoRe.MatchString(repo) && !seen[repo] {
					seen[repo] = true
					cfg.Allow.Repos = append(cfg.Allow.Repos, repo)
				}
			}
			sort.Strings(cfg.Allow.Repos)
			// Short paths only for listed repositories, one segment each; collisions are dropped
			// when publishing (watcher.aliases).
			cfg.Allow.Aliases = map[string][]string{}
			for repo, as := range req.Allow.Aliases {
				repo = strings.ToLower(repo)
				if !seen[repo] {
					continue
				}
				for _, a := range as {
					if a = strings.ToLower(a); validAlias(a) {
						cfg.Allow.Aliases[repo] = append(cfg.Allow.Aliases[repo], a)
					}
				}
			}
		}
		if req.Public != nil {
			cfg.Public = req.Public
		}
		writeJSON(filepath.Join(a.stateDir, "config.json"), cfg)
		if req.Public != nil && theWatcher != nil {
			theWatcher.syncPublic(cfg)
		}
		if nameChanged {
			a.forget()
			if wt, err := publisherWatcher(); err == nil {
				ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
				a.checkName(ctx, wt)
				cancel()
			}
		}
		if req.Allow != nil && theWatcher != nil {
			theWatcher.wake() // a repository allowed again is imported now
		}
	}
	cfg := loadConfig(a.stateDir)
	reply(w, map[string]any{"config": cfg, "public": cfg.publicOn(), "allowRequired": cfg.Allow.Required || os.Getenv("IMPORT_ALLOW_REQUIRED") == "true",
		"lifetime": ipnsLifetime, "ttl": ipnsTTL})
}

func (a *admin) handleRepublish(w http.ResponseWriter, r *http.Request) {
	wt, err := publisherWatcher()
	if err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	name, err := updateTree(r.Context(), wt.publisher, publisherDir(wt.publisher), 0, nil)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	a.forget()
	reply(w, map[string]any{"published": name})
}

// ---------------------------------------------------------------- key restore

// handleKeyImport makes a backed-up key the publisher key again: the way to move a forge to a new
// server, or to recover one, keeping its name.
//
//  1. Decrypt the backup; the key must be the one it names.
//  2. Find the name's sequence number on the network. Kubo only knows its own, and a record with a
//     lower number than the network's loses: refuse when the network cannot be asked, unless the
//     caller gives a sequence.
//  3. Keep the current key, renamed <publisher>-replaced-<time> (never deleted).
//  4. Import the key under the publisher's name.
//  5. If this node has nothing published yet and the network has a tree, adopt that tree (a
//     restore on a fresh server); otherwise publish this node's tree under the restored name.
//  6. Publish with sequence = network's + 1 and rewrite published.json.
func (a *admin) handleKeyImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Backup     keyBackup `json:"backup"`
		Passphrase string    `json:"passphrase"`
		Sequence   uint64    `json:"sequence"`
	}
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	wt, err := publisherWatcher()
	if err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	key, err := openKey(&req.Backup, req.Passphrase)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	id := req.Backup.ID
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()

	current, err := keyID(ctx, wt.publisher)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if current == id {
		// Already in place, maybe from an attempt that failed after the import: make sure
		// published.json and the record follow it.
		wt.mu.Lock()
		wt.pub.Publisher = id
		for repo, pr := range wt.pub.Images {
			pr.IPNS = id + "/" + repo
		}
		wt.save()
		wt.mu.Unlock()
		a.forget()
		if _, err := updateTree(ctx, wt.publisher, publisherDir(wt.publisher), 0, nil); err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		reply(w, map[string]any{"id": id, "status": "this key is already the publisher key: published again"})
		return
	}

	net, netErr := networkRecord(ctx, id)
	var seq uint64
	switch {
	case req.Sequence > 0:
		seq = req.Sequence
	case netErr != nil:
		fail(w, http.StatusServiceUnavailable, fmt.Errorf("cannot read the name's current record from the network (%v): retry, or give a sequence number above the network's", netErr))
		return
	case net != nil:
		seq = net.Sequence + 1
	}
	// This node may hold a newer record of the name than the network shows (it published it
	// before), and Kubo refuses to publish below its own.
	if local, err := localRecord(ctx, id); err == nil && local.Sequence+1 > seq && req.Sequence == 0 {
		seq = local.Sequence + 1
	}

	keys, err := listKeys(ctx)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	copies := map[string][]string{} // id → names
	for _, k := range keys {
		copies[k.Id] = append(copies[k.Id], k.Name)
	}
	replaced := ""
	if current != "" {
		if len(copies[current]) > 1 {
			// Another name holds the same key (an earlier restore kept it): no need for a second copy.
			if err := kuboJSON(ctx, "key/rm", arg(wt.publisher), nil); err != nil {
				fail(w, http.StatusInternalServerError, err)
				return
			}
		} else {
			replaced = fmt.Sprintf("%s-replaced-%s", wt.publisher, time.Now().UTC().Format("20060102T150405Z"))
			if err := kuboJSON(ctx, "key/rename", arg(wt.publisher, replaced), nil); err != nil {
				fail(w, http.StatusInternalServerError, err)
				return
			}
			log.Printf("admin: key %s (%s) kept as %s", wt.publisher, current, replaced)
		}
	}
	if have := copies[id]; len(have) > 0 {
		// This node still has the key under another name (kept by an earlier restore): use it.
		err = kuboJSON(ctx, "key/rename", arg(have[0], wt.publisher), nil)
	} else {
		var imported struct{ Name, Id string }
		err = kuboUpload(ctx, "key/import", url.Values{"arg": {wt.publisher}, "ipns-base": {"base36"}, "format": {backupFormat}}, key, &imported)
		if err == nil && imported.Id != id {
			err = fmt.Errorf("imported key is %s, expected %s", imported.Id, id)
		}
	}
	if err != nil {
		if replaced != "" { // put the previous key back
			kuboJSON(ctx, "key/rm", arg(wt.publisher), nil)
			kuboJSON(ctx, "key/rename", arg(replaced, wt.publisher), nil)
		}
		fail(w, http.StatusInternalServerError, err)
		return
	}

	// From here the key is the publisher's: published.json follows it whatever happens next.
	wt.mu.Lock()
	wt.pub.Publisher, wt.pub.Name, wt.pub.NameVerifiedAt = id, "", ""
	for repo, pr := range wt.pub.Images {
		pr.IPNS = id + "/" + repo
	}
	wt.save()
	wt.mu.Unlock()
	a.forget()

	base := publisherDir(wt.publisher)
	adopted := false
	_, err = updateTree(ctx, wt.publisher, base, seq, func(base string) error {
		var ls struct{ Entries []json.RawMessage }
		kuboJSON(ctx, "files/ls", arg(base), &ls)
		if len(ls.Entries) > 0 || net == nil || !strings.HasPrefix(net.Value, "/ipfs/") {
			return nil
		}
		if err := kuboJSON(ctx, "files/rm", url.Values{"arg": {base}, "recursive": {"true"}, "force": {"true"}}, nil); err != nil {
			return err
		}
		adopted = true
		return kuboJSON(ctx, "files/cp", arg(net.Value, base), nil)
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, fmt.Errorf("key imported, publishing failed (Republish to retry): %w", err))
		return
	}

	if adopted {
		wt.mu.Lock()
		wt.pub.Images = map[string]*publishedRepo{}
		wt.mu.Unlock()
		walkTree(ctx, base, "", func(repo, tag, cid string) {
			wt.mu.Lock()
			wt.pub.set(repo, id+"/"+repo, tag, publishedTag{CID: cid})
			wt.mu.Unlock()
		})
	}
	wt.mu.Lock()
	wt.save()
	wt.mu.Unlock()
	a.forget()
	go a.checkName(context.Background(), wt)
	log.Printf("admin: publisher key is now %s (sequence %d, adopted network tree: %v)", id, seq, adopted)
	reply(w, map[string]any{"id": id, "replaced": replaced, "previous": current, "sequence": seq, "adopted": adopted})
}

// walkTree calls fn for every tag of a publisher tree: directories are image paths, files are
// tags (an image root is a file).
func walkTree(ctx context.Context, dir, repo string, fn func(repo, tag, cid string)) {
	var ls struct {
		Entries []struct {
			Name string
			Type int
			Hash string
		}
	}
	if err := kuboJSON(ctx, "files/ls", url.Values{"arg": {dir}, "long": {"true"}}, &ls); err != nil {
		log.Printf("admin: list %s: %v", dir, err)
		return
	}
	for _, e := range ls.Entries {
		if e.Type == 1 {
			walkTree(ctx, dir+"/"+e.Name, strings.TrimPrefix(repo+"/"+e.Name, "/"), fn)
		} else if repo != "" {
			fn(repo, e.Name, e.Hash)
		}
	}
}
