package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A key made by Kubo v0.43.1 (`ipfs key gen t --type ed25519 --ipns-base base36`, then
// `ipfs key export t`), and the name Kubo printed for it.
const (
	vectorKey = "CAESQEgAFnJaC//4NMPXmGjBx4fMcn1cRGFhAV2reN7GqcyS+z5e/JsuHUwhZneaa52YACcp2T6p2ri/FfdyILTLfWo="
	vectorID  = "k51qzi5uqu5dmg0qkz1yhcmegr2nf4jc7q8v9xw1mqv2p6211xzntxps7n6dm2"
	// sealKey("forge", vectorKey, vectorPass): the same vector is in bridge/bridge_test.go, so the
	// bridge's backups open here.
	vectorPass   = "correct horse battery staple"
	vectorBackup = `{"v":1,"kdf":"pbkdf2-sha256","iter":600000,"salt":"5mqcBpjiFkE3IVdNeqL6Bw==","nonce":"xKVdz0QswnMpkRz3","name":"forge","id":"k51qzi5uqu5dmg0qkz1yhcmegr2nf4jc7q8v9xw1mqv2p6211xzntxps7n6dm2","format":"libp2p-protobuf-cleartext","ct":"k9Hq6zuS1jmKA13OeIZpGuK4M5pUIKA0F4n0mQrFq5J8E/Q+RlRzu91PoTJGiwR/zUeOG14uO138MG3pPVqTzrFklRcN21yEGyeImaUf0EdWJPUX"}`
)

func TestIPNSName(t *testing.T) {
	key, _ := base64.StdEncoding.DecodeString(vectorKey)
	id, err := ipnsName(key)
	if err != nil || id != vectorID {
		t.Fatalf("ipnsName = %q, %v; want %s", id, err, vectorID)
	}
	bad := append([]byte{}, key...)
	bad[len(bad)-1] ^= 1 // public half no longer matches the seed
	if _, err := ipnsName(bad); err == nil {
		t.Error("mismatched public key accepted")
	}
	if _, err := ipnsName([]byte{0x08, 0x00, 0x12, 0x00}); err == nil {
		t.Error("RSA type accepted")
	}
}

func TestKeyBackup(t *testing.T) {
	key, _ := base64.StdEncoding.DecodeString(vectorKey)
	var fixed keyBackup
	if err := json.Unmarshal([]byte(vectorBackup), &fixed); err != nil {
		t.Fatal(err)
	}
	if got, err := openKey(&fixed, vectorPass); err != nil || !bytes.Equal(got, key) {
		t.Fatalf("fixed vector: %v", err)
	}
	b, err := sealKey("forge", key, "another long passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != vectorID || b.Name != "forge" {
		t.Errorf("sealed as %s/%s", b.Name, b.ID)
	}
	if _, err := openKey(b, "wrong passphrase!!"); err == nil {
		t.Error("wrong passphrase accepted")
	}
	renamed := *b
	renamed.Name = "other"
	if _, err := openKey(&renamed, "another long passphrase"); err == nil {
		t.Error("edited name accepted")
	}
	if _, err := sealKey("forge", key, "short"); err == nil {
		t.Error("short passphrase accepted")
	}
}

func TestConfigAllowed(t *testing.T) {
	var c config
	if !c.allowed("a/b") {
		t.Error("no allowlist: everything is allowed")
	}
	c.Allow.Repos = []string{"owner/app"}
	if !c.allowed("Owner/App") || c.allowed("owner/other") {
		t.Error("allowlist")
	}
	c.Allow.Repos = nil
	c.Allow.Required = true
	if c.allowed("owner/app") {
		t.Error("required and empty: nothing is allowed")
	}
	t.Setenv("IMPORT_ALLOW_REQUIRED", "true")
	if (config{}).allowed("owner/app") {
		t.Error("IMPORT_ALLOW_REQUIRED and empty: nothing is allowed")
	}
}

// fakeRegistry is a source registry: repo → tag → digest, recording deletes.
type fakeRegistry struct {
	mu      sync.Mutex
	tags    map[string]map[string]string
	deleted []string
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/v2/")
	switch {
	case p == "_catalog":
		var repos []string
		for repo := range f.tags {
			repos = append(repos, repo)
		}
		json.NewEncoder(w).Encode(map[string]any{"repositories": repos})
	case strings.HasSuffix(p, "/tags/list"):
		repo := strings.TrimSuffix(p, "/tags/list")
		var tags []string
		for tag := range f.tags[repo] {
			tags = append(tags, tag)
		}
		json.NewEncoder(w).Encode(map[string]any{"name": repo, "tags": tags})
	case strings.Contains(p, "/manifests/"):
		i := strings.LastIndex(p, "/manifests/")
		repo, ref := p[:i], p[i+len("/manifests/"):]
		if r.Method == "DELETE" {
			for tag, d := range f.tags[repo] {
				if d == ref {
					delete(f.tags[repo], tag)
				}
			}
			f.deleted = append(f.deleted, repo+"@"+ref)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		d, ok := f.tags[repo][ref]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Docker-Content-Digest", d)
	default:
		http.NotFound(w, r)
	}
}

// A pass over a registry whose tags are already published: the allowlist skips one repository, and
// the cleanup deletes a digest only once every tag on it is settled.
func TestPassAllowlistAndCleanup(t *testing.T) {
	reg := &fakeRegistry{tags: map[string]map[string]string{
		"owner/app":   {"1.0.0": "sha256:aa", "latest": "sha256:aa", "0.9.0": "sha256:99"},
		"owner/other": {"1.0.0": "sha256:bb"},
	}}
	srv := httptest.NewServer(reg)
	defer srv.Close()
	dir := t.TempDir()
	w, err := newWatcher(srv.URL, time.Minute, dir)
	if err != nil {
		t.Fatal(err)
	}
	w.publisher = "forge"
	// 1.0.0 and latest are published; 0.9.0 was unpublished by the admin.
	for _, tag := range []string{"1.0.0", "latest"} {
		w.state[w.stateKey("owner/app", tag)] = "sha256:aa"
		w.pub.set("owner/app", "k51x/owner/app", tag, publishedTag{CID: "bafy", Digest: "sha256:aa"})
	}
	w.state[w.stateKey("owner/app", "0.9.0")] = tombstone + "sha256:99"
	writeJSON(filepath.Join(dir, "config.json"), map[string]any{"allow": map[string]any{"required": true, "repos": []string{"owner/app"}}})

	old := cleanupStaging
	cleanupStaging = true
	defer func() { cleanupStaging = old }()
	w.pass(context.Background())

	got := strings.Join(reg.deleted, " ")
	if !strings.Contains(got, "owner/app@sha256:aa") || !strings.Contains(got, "owner/app@sha256:99") {
		t.Errorf("deleted %q: want both settled digests of owner/app", got)
	}
	if strings.Contains(got, "owner/other") {
		t.Errorf("deleted %q: owner/other is not allowed, so not settled", got)
	}
	if !w.skipped["owner/other"] {
		t.Error("owner/other not reported as skipped")
	}
}

// fakeKubo answers the Kubo RPC calls the key restore makes, and records them.
type fakeKubo struct {
	mu    sync.Mutex
	keys  map[string]string // name → id
	calls []string
	seq   string // the sequence name/publish was given
}

func (k *fakeKubo) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	k.mu.Lock()
	defer k.mu.Unlock()
	cmd := strings.TrimPrefix(r.URL.Path, "/api/v0/")
	q := r.URL.Query()
	k.calls = append(k.calls, cmd+" "+strings.Join(q["arg"], " "))
	switch cmd {
	case "key/list":
		var keys []map[string]string
		for n, id := range k.keys {
			keys = append(keys, map[string]string{"Name": n, "Id": id})
		}
		json.NewEncoder(w).Encode(map[string]any{"Keys": keys})
	case "key/rename":
		a := q["arg"]
		k.keys[a[1]] = k.keys[a[0]]
		delete(k.keys, a[0])
		json.NewEncoder(w).Encode(map[string]any{"Was": a[0], "Now": a[1]})
	case "key/rm":
		delete(k.keys, q.Get("arg"))
		w.Write([]byte("{}"))
	case "key/import":
		r.ParseMultipartForm(1 << 20)
		f, _, _ := r.FormFile("file")
		raw, _ := io.ReadAll(f)
		id, _ := ipnsName(raw)
		k.keys[q.Get("arg")] = id
		json.NewEncoder(w).Encode(map[string]string{"Name": q.Get("arg"), "Id": id})
	case "name/inspect":
		json.NewEncoder(w).Encode(map[string]any{"Entry": map[string]any{"Value": "/ipfs/bafyremote", "Sequence": 7},
			"Validation": map[string]any{"Valid": true}})
	case "files/ls":
		json.NewEncoder(w).Encode(map[string]any{"Entries": []any{map[string]any{"Name": "owner", "Type": 1}}})
	case "files/stat":
		json.NewEncoder(w).Encode(map[string]string{"Hash": "bafytree"})
	case "name/publish":
		k.seq = q.Get("sequence")
		json.NewEncoder(w).Encode(map[string]string{"Name": k.keys[q.Get("key")]})
	default: // files/mkdir, pin/add, pin/rm, …
		w.Write([]byte("{}"))
	}
}

func TestKeyImport(t *testing.T) {
	kubo := &fakeKubo{keys: map[string]string{"forge": "k51old"}}
	ksrv := httptest.NewServer(kubo)
	defer ksrv.Close()
	// The network has a record for the restored name, sequence 7 (fakeKubo's name/inspect).
	dsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/routing/v1/ipns/"+vectorID {
			// What delegated-ipfs.dev answers for a name it does not know.
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Write([]byte("delegate error: routing: not found\n"))
			return
		}
		w.Header().Set("Content-Type", "application/vnd.ipfs.ipns-record")
		w.Write([]byte("record"))
	}))
	defer dsrv.Close()
	oldKubo, oldDelegated, oldWatcher := kuboAPI, delegated, theWatcher
	kuboAPI, delegated = ksrv.URL, dsrv.URL
	defer func() { kuboAPI, delegated, theWatcher = oldKubo, oldDelegated, oldWatcher }()

	dir := t.TempDir()
	w, _ := newWatcher("http://staging:5000", time.Minute, dir)
	w.publisher = "forge"
	w.pub.Publisher = "k51old"
	w.pub.set("owner/app", "k51old/owner/app", "1.0.0", publishedTag{CID: "bafyimg"})
	theWatcher = w
	a := &admin{stateDir: dir, token: []byte("t")}

	body, _ := json.Marshal(map[string]any{"backup": json.RawMessage(vectorBackup), "passphrase": vectorPass})
	rec := httptest.NewRecorder()
	a.handleKeyImport(rec, httptest.NewRequest("POST", "/admin/key/import", bytes.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("import: %d %s", rec.Code, rec.Body)
	}
	if kubo.keys["forge"] != vectorID {
		t.Errorf("forge is %s", kubo.keys["forge"])
	}
	kept := false
	for n, id := range kubo.keys {
		kept = kept || (strings.HasPrefix(n, "forge-replaced-") && id == "k51old")
	}
	if !kept {
		t.Errorf("previous key not kept: %v", kubo.keys)
	}
	if kubo.seq != "8" {
		t.Errorf("published with sequence %q, want 8 (network's 7 + 1)", kubo.seq)
	}
	if w.pub.Publisher != vectorID || w.pub.Images["owner/app"].IPNS != vectorID+"/owner/app" {
		t.Errorf("published.json not rewritten: %+v", w.pub)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "published.json"))
	if !strings.Contains(string(b), vectorID) {
		t.Error("published.json not saved")
	}

	// A wrong passphrase changes nothing.
	body, _ = json.Marshal(map[string]any{"backup": json.RawMessage(vectorBackup), "passphrase": "nope nope nope"})
	rec = httptest.NewRecorder()
	a.handleKeyImport(rec, httptest.NewRequest("POST", "/admin/key/import", bytes.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("wrong passphrase: %d", rec.Code)
	}
}

func TestAdminAuth(t *testing.T) {
	a := &admin{token: []byte("secret-token")}
	h := a.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	for _, c := range []struct {
		header string
		code   int
	}{{"", 401}, {"Bearer wrong", 401}, {"secret-token", 401}, {"Bearer secret-token", 200}} {
		r := httptest.NewRequest("GET", "/admin/status", nil)
		if c.header != "" {
			r.Header.Set("Authorization", c.header)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != c.code {
			t.Errorf("%q: %d, want %d", c.header, rec.Code, c.code)
		}
	}
}

func TestAdminToken(t *testing.T) {
	dir := t.TempDir()
	a, err := adminToken(dir)
	if err != nil || len(a) != 64 {
		t.Fatalf("%q %v", a, err)
	}
	b, _ := adminToken(dir)
	if !bytes.Equal(a, b) {
		t.Error("token changed on restart")
	}
}

func TestRepoTagValid(t *testing.T) {
	for _, ok := range []repoTag{{Repo: "owner/app", Tag: "1.0.0"}, {Repo: "a/b/c", Tag: "latest", From: "v2"}} {
		if ok.valid() != nil {
			t.Errorf("%+v rejected", ok)
		}
	}
	for _, bad := range []repoTag{{Repo: "../x", Tag: "1"}, {Repo: "owner/app", Tag: "../1"}, {Repo: "Owner/app", Tag: "1"}, {Repo: "a//b", Tag: "1"}} {
		if bad.valid() == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

func TestNetworkRecordNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("delegate error: routing: not found\n"))
	}))
	defer srv.Close()
	old := delegated
	delegated = srv.URL
	defer func() { delegated = old }()
	if rec, err := networkRecord(context.Background(), "k51x"); rec != nil || err != nil {
		t.Errorf("unknown name: %v %v, want no record and no error", rec, err)
	}
}

// Restoring a key this node still holds under another name (kept by an earlier restore) reuses it,
// and the outgoing key, which has another copy, is dropped instead of kept twice.
func TestKeyImportReusesKeptKey(t *testing.T) {
	kubo := &fakeKubo{keys: map[string]string{"forge": "k51other", "forge-replaced-1": vectorID, "forge-replaced-2": "k51other"}}
	ksrv := httptest.NewServer(kubo)
	defer ksrv.Close()
	dsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	defer dsrv.Close()
	oldKubo, oldDelegated, oldWatcher := kuboAPI, delegated, theWatcher
	kuboAPI, delegated = ksrv.URL, dsrv.URL
	defer func() { kuboAPI, delegated, theWatcher = oldKubo, oldDelegated, oldWatcher }()
	w, _ := newWatcher("http://staging:5000", time.Minute, t.TempDir())
	w.publisher = "forge"
	theWatcher = w
	a := &admin{stateDir: t.TempDir()}
	body, _ := json.Marshal(map[string]any{"backup": json.RawMessage(vectorBackup), "passphrase": vectorPass})
	rec := httptest.NewRecorder()
	a.handleKeyImport(rec, httptest.NewRequest("POST", "/admin/key/import", bytes.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if kubo.keys["forge"] != vectorID || len(kubo.keys) != 2 {
		t.Errorf("keys %v: want forge = the kept key, and k51other once", kubo.keys)
	}
	for _, c := range kubo.calls {
		if strings.HasPrefix(c, "key/import") {
			t.Error("imported a second copy")
		}
	}
}

func TestAliasesCollisions(t *testing.T) {
	w, _ := newWatcher("http://staging:5000", time.Minute, t.TempDir())
	var c config
	c.Allow.Repos = []string{"metadec/app", "metadec/tools", "metadec/alice", "alice/thing", "metadec/x", "metadec/y"}
	c.Allow.Aliases = map[string][]string{
		"metadec/app":   {"app"},
		"metadec/tools": {"Tools"}, // lowercased
		"metadec/alice": {"alice"}, // alice/ is another owner's folder
		"metadec/x":     {"same"},
		"metadec/y":     {"same"}, // claimed by metadec/x first
		"metadec/bad":   {"a/b"},
	}
	got := w.aliases(c)
	want := map[string][]string{"metadec/app": {"app"}, "metadec/tools": {"tools"}, "metadec/x": {"same"}}
	if len(got) != len(want) {
		t.Fatalf("aliases %v, want %v", got, want)
	}
	for r, a := range want {
		if strings.Join(got[r], ",") != strings.Join(a, ",") {
			t.Errorf("%s: %v, want %v", r, got[r], a)
		}
	}
	// A published repository's owner folder is taken too.
	w.pub.set("app/server", "k51/app/server", "1", publishedTag{CID: "c"})
	if a := w.aliases(c)["metadec/app"]; len(a) != 0 {
		t.Errorf("alias app collides with published app/server: %v", a)
	}
}

func TestSyncAliases(t *testing.T) {
	kubo := &fakeKubo{keys: map[string]string{"forge": "k51forge"}}
	ksrv := httptest.NewServer(kubo)
	defer ksrv.Close()
	old := kuboAPI
	kuboAPI = ksrv.URL
	defer func() { kuboAPI = old }()
	w, _ := newWatcher("http://staging:5000", time.Minute, t.TempDir())
	w.publisher = "forge"
	w.pub.set("metadec/app", "k51forge/metadec/app", "1.0.0", publishedTag{CID: "bafy1"})
	w.pub.set("metadec/app", "k51forge/metadec/app", "latest", publishedTag{CID: "bafy1"})
	var c config
	c.Allow.Repos = []string{"metadec/app"}
	c.Allow.Aliases = map[string][]string{"metadec/app": {"app"}}

	w.syncAliases(context.Background(), c)
	cps := 0
	for _, call := range kubo.calls {
		if strings.HasPrefix(call, "files/cp /ipfs/bafy1 /publishers/forge/app/") {
			cps++
		}
	}
	if cps != 2 || strings.Join(w.pub.Images["metadec/app"].Aliases, ",") != "app" {
		t.Fatalf("add: %d copies, aliases %v; calls %v", cps, w.pub.Images["metadec/app"].Aliases, kubo.calls)
	}

	// The root organisation changes: the short path goes.
	kubo.calls = nil
	c.Allow.Aliases = nil
	w.syncAliases(context.Background(), c)
	removed := false
	for _, call := range kubo.calls {
		removed = removed || call == "files/rm /publishers/forge/app"
	}
	if !removed || len(w.pub.Images["metadec/app"].Aliases) != 0 {
		t.Errorf("drop: removed %v, aliases %v; calls %v", removed, w.pub.Images["metadec/app"].Aliases, kubo.calls)
	}
}

func TestConfigAliasesOnlyForListedRepos(t *testing.T) {
	dir := t.TempDir()
	a := &admin{stateDir: dir}
	body := `{"allow":{"required":true,"repos":["Metadec/App"],"aliases":{"metadec/app":["App"],"other/x":["x"],"metadec/app2":["a/b"]}}}`
	rec := httptest.NewRecorder()
	a.handleConfig(rec, httptest.NewRequest("PATCH", "/admin/config", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	c := loadConfig(dir)
	if len(c.Allow.Aliases) != 1 || strings.Join(c.Allow.Aliases["metadec/app"], ",") != "app" {
		t.Errorf("aliases %v", c.Allow.Aliases)
	}
}

func TestEnsureImportAuth(t *testing.T) {
	f := filepath.Join(t.TempDir(), "staging-auth")
	t.Setenv("IMPORT_AUTH_FILE", f)
	t.Setenv("IMPORT_AUTH_GENERATE", "true")
	if err := ensureImportAuth(); err != nil {
		t.Fatal(err)
	}
	a := importAuth()
	if !strings.HasPrefix(a, "ipcr:") || len(a) < 40 {
		t.Fatalf("generated %q", a)
	}
	ensureImportAuth()
	if importAuth() != a {
		t.Error("regenerated on restart")
	}
}
