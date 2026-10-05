package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sign(secret, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return hex.EncodeToString(m.Sum(nil))
}

func TestValidSignature(t *testing.T) {
	body := `{"repository":{"id":7}}`
	if !validSignature([]byte("s3cret"), []byte(body), sign("s3cret", body)) {
		t.Error("good signature rejected")
	}
	for _, sig := range []string{"", "zz", sign("other", body), sign("s3cret", body+" ")} {
		if validSignature([]byte("s3cret"), []byte(body), sig) {
			t.Errorf("bad signature %q accepted", sig)
		}
	}
}

func TestHook(t *testing.T) {
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "hook-secret")
	var got []int64
	h := &hookHandler{secretFile: secretFile, enqueue: func(id int64) { got = append(got, id) }}
	post := func(body, sig string) int {
		r := httptest.NewRequest("POST", "/hooks/gitea", strings.NewReader(body))
		r.Header.Set("X-Gitea-Signature", sig)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	body := `{"ref":"refs/tags/v1.0.0","repository":{"id":42,"full_name":"a/b"}}`
	if c := post(body, sign("k", body)); c != http.StatusServiceUnavailable {
		t.Errorf("no secret yet: %d", c)
	}
	os.WriteFile(secretFile, []byte("k\n"), 0o600)
	if c := post(body, sign("x", body)); c != http.StatusUnauthorized {
		t.Errorf("bad signature: %d", c)
	}
	if c := post(body, sign("k", body)); c != http.StatusAccepted {
		t.Errorf("good delivery: %d", c)
	}
	if c := post(`{"sender":{}}`, sign("k", `{"sender":{}}`)); c != http.StatusNoContent {
		t.Errorf("no repository: %d", c)
	}
	if len(got) != 1 || got[0] != 42 {
		t.Errorf("enqueued %v, want [42]", got)
	}
}

func TestQueueCoalesces(t *testing.T) {
	q := newQueue()
	for _, id := range []int64{3, 1, 3, 3, 2, 1} {
		q.add(id)
	}
	var order []int64
	for {
		id, ok := q.next()
		if !ok {
			break
		}
		order = append(order, id)
	}
	if len(order) != 3 || order[0] != 3 || order[1] != 1 || order[2] != 2 {
		t.Errorf("order %v, want [3 1 2]", order)
	}
	q.add(3) // popped, so it may be queued again
	if id, ok := q.next(); !ok || id != 3 {
		t.Error("re-adding a popped repository")
	}
}

func TestPublic(t *testing.T) {
	r := repo{}
	if !r.public() {
		t.Error("plain repository")
	}
	for _, f := range []func(*repo){
		func(r *repo) { r.Private = true },
		func(r *repo) { r.Internal = true },
		func(r *repo) { r.Owner.Visibility = "limited" },
		func(r *repo) { r.Owner.Visibility = "private" },
	} {
		r := repo{}
		f(&r)
		if r.public() {
			t.Errorf("%+v counted as public", r)
		}
	}
}

func TestRIDFromURL(t *testing.T) {
	for in, want := range map[string]string{
		"rad://z2pnYhouQzbiTEmXW9bydgXFqjP1o\n":                                                "rad:z2pnYhouQzbiTEmXW9bydgXFqjP1o",
		"rad://z2pnYhouQzbiTEmXW9bydgXFqjP1o/z6MkppQoVh2QKyQGkPzGgEaqZcXmbeBVhZseQ2cgxuMzA5gR": "rad:z2pnYhouQzbiTEmXW9bydgXFqjP1o",
		"https://example.com/x.git":                                                            "",
		"":                                                                                     "",
	} {
		if got := ridFromURL(in); got != want {
			t.Errorf("ridFromURL(%q) = %q, want %q", in, got, want)
		}
	}
	if got := ridRe.FindString("Your Repository ID (RID) is rad:z2pnYhouQzbiTEmXW9bydgXFqjP1o\nYou can"); got != "rad:z2pnYhouQzbiTEmXW9bydgXFqjP1o" {
		t.Errorf("ridRe: %q", got)
	}
}

func TestPushedRefs(t *testing.T) {
	out := "To rad://z2pn/z6Mk\n" +
		"=\trefs/heads/main:refs/heads/main\t[up to date]\n" +
		"*\trefs/tags/v1.0.0:refs/tags/v1.0.0\t[new tag]\n" +
		"-\t:refs/heads/old\t[deleted]\n" +
		"+\trefs/heads/dev:refs/heads/dev\t1111111...2222222 (forced update)\n" +
		"Done\n"
	got := strings.Join(pushedRefs(out), "; ")
	want := "tags/v1.0.0 [new tag]; old [deleted]; dev 1111111...2222222 (forced update)"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repos.json")
	s, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.known(5) || s.get(5).ID != 5 {
		t.Fatal("empty state")
	}
	s.put(repoState{ID: 5, FullName: "b/two", RID: "rad:z2", State: stSynced, Refs: "abc", Crefs: true})
	s.put(repoState{ID: 9, FullName: "a/one", State: stWaiting})
	s2, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if r := s2.get(5); r.RID != "rad:z2" || r.Refs != "abc" || !r.Crefs || r.State != stSynced {
		t.Errorf("reloaded %+v", r)
	}
	l := s2.list()
	if len(l) != 2 || l[0].FullName != "a/one" || l[1].Refs != "" {
		t.Errorf("list %+v", l)
	}
	if ids := s2.ids(); len(ids) != 2 || ids[0] != 5 || ids[1] != 9 {
		t.Errorf("ids %v", ids)
	}
}

func TestExampleEmbedded(t *testing.T) {
	for _, p := range []string{"example/Dockerfile", "example/.gitea/workflows/build.yml", "example/README.md"} {
		if _, err := exampleFiles.ReadFile(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	b, _ := exampleFiles.ReadFile("example/.gitea/workflows/build.yml")
	if !strings.Contains(string(b), "localhost:5000/${{ github.repository }}") || !strings.Contains(string(b), "network=host") ||
		!strings.Contains(string(b), "secrets.IPCR_PUSH_TOKEN") {
		t.Error("example workflow lost its staging registry lines")
	}
}

func TestWeb(t *testing.T) {
	dir := t.TempDir()
	st, _ := loadState(filepath.Join(dir, "repos.json"))
	h := webHandler(st, filepath.Join(dir, "published.json"))
	get := func(p string) (int, string) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		return w.Code, w.Body.String()
	}
	if c, b := get("/"); c != 200 || !strings.Contains(b, "IPCR Forge") {
		t.Errorf("/: %d", c)
	}
	if c, _ := get("/images.json"); c != 404 {
		t.Errorf("/images.json before anything is published: %d", c)
	}
	os.WriteFile(filepath.Join(dir, "published.json"), []byte(`{"images":{}}`), 0o644)
	if c, b := get("/images.json"); c != 200 || b != `{"images":{}}` {
		t.Errorf("/images.json: %d %q", c, b)
	}
	if c, b := get("/repos.json"); c != 200 || strings.TrimSpace(b) != "[]" {
		t.Errorf("/repos.json: %d %q", c, b)
	}
	if c, _ := get("/nothing"); c != 404 {
		t.Errorf("/nothing: %d", c)
	}
}

// The same vector as gateway/admin_test.go: a Kubo key and its name, and a backup of it that ipcrd
// must be able to open.
const (
	vectorKey    = "CAESQEgAFnJaC//4NMPXmGjBx4fMcn1cRGFhAV2reN7GqcyS+z5e/JsuHUwhZneaa52YACcp2T6p2ri/FfdyILTLfWo="
	vectorID     = "k51qzi5uqu5dmg0qkz1yhcmegr2nf4jc7q8v9xw1mqv2p6211xzntxps7n6dm2"
	vectorPass   = "correct horse battery staple"
	vectorBackup = `{"v":1,"kdf":"pbkdf2-sha256","iter":600000,"salt":"5mqcBpjiFkE3IVdNeqL6Bw==","nonce":"xKVdz0QswnMpkRz3","name":"forge","id":"k51qzi5uqu5dmg0qkz1yhcmegr2nf4jc7q8v9xw1mqv2p6211xzntxps7n6dm2","format":"libp2p-protobuf-cleartext","ct":"k9Hq6zuS1jmKA13OeIZpGuK4M5pUIKA0F4n0mQrFq5J8E/Q+RlRzu91PoTJGiwR/zUeOG14uO138MG3pPVqTzrFklRcN21yEGyeImaUf0EdWJPUX"}`
)

func TestKeyBackupVector(t *testing.T) {
	key, _ := base64.StdEncoding.DecodeString(vectorKey)
	var fixed keyBackup
	json.Unmarshal([]byte(vectorBackup), &fixed)
	if got, err := openKey(&fixed, vectorPass); err != nil || !bytes.Equal(got, key) {
		t.Fatalf("fixed vector: %v", err)
	}
	b, err := sealKey("forge", key, "a passphrase long enough")
	if err != nil || b.ID != vectorID {
		t.Fatalf("seal: %v %s", err, b.ID)
	}
	if got, err := openKey(b, "a passphrase long enough"); err != nil || !bytes.Equal(got, key) {
		t.Fatalf("round trip: %v", err)
	}
}

func TestKeystoreFile(t *testing.T) {
	if got := keystoreFile("/ks", "forge"); got != "/ks/key_mzxxez3f" {
		t.Errorf("forge → %s, want /ks/key_mzxxez3f", got)
	}
}

func TestExport(t *testing.T) {
	dir := t.TempDir()
	key, _ := base64.StdEncoding.DecodeString(vectorKey)
	os.WriteFile(keystoreFile(dir, "forge"), key, 0o400)
	au, _ := newAuth([]string{"ipcr-forge-x.example"}, "http://gitea", dir, filepath.Join(dir, "session-key"))
	a := &adminAPI{keystore: dir, publisher: "forge", au: au}
	r := httptest.NewRequest("POST", "/admin/api/key/export", strings.NewReader(`{"passphrase":"long enough passphrase"}`))
	w := httptest.NewRecorder()
	a.export(w, r)
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Disposition"), ".ipcrkey.json") {
		t.Fatalf("export: %d %s", w.Code, w.Body)
	}
	var b keyBackup
	json.Unmarshal(w.Body.Bytes(), &b)
	if got, err := openKey(&b, "long enough passphrase"); err != nil || !bytes.Equal(got, key) || b.ID != vectorID {
		t.Fatalf("exported backup does not open: %v", err)
	}
	w = httptest.NewRecorder()
	a.export(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"passphrase":"short"}`)))
	if w.Code != 400 {
		t.Errorf("short passphrase: %d", w.Code)
	}
}

func TestSession(t *testing.T) {
	dir := t.TempDir()
	au, err := newAuth(nil, "", dir, filepath.Join(dir, "k"))
	if err != nil {
		t.Fatal(err)
	}
	req := func(v string) *http.Request {
		r := httptest.NewRequest("GET", "/admin", nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: v})
		return r
	}
	good := au.sign("root", time.Now().Add(time.Hour))
	if u, err := au.user(req(good)); err != nil || u != "root" {
		t.Errorf("good session: %q %v", u, err)
	}
	if _, err := au.user(req(au.sign("root", time.Now().Add(-time.Minute)))); err == nil {
		t.Error("expired session accepted")
	}
	forged := base64.RawURLEncoding.EncodeToString([]byte("eve|99999999999")) + good[strings.Index(good, "."):]
	if _, err := au.user(req(forged)); err == nil {
		t.Error("edited session accepted")
	}
	// The key survives a restart.
	au2, _ := newAuth(nil, "", dir, filepath.Join(dir, "k"))
	if _, err := au2.user(req(good)); err != nil {
		t.Error("session lost after restart")
	}
}

func TestRequireAdmin(t *testing.T) {
	dir := t.TempDir()
	au, _ := newAuth(nil, "", dir, filepath.Join(dir, "k"))
	h := au.requireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	cookie := &http.Cookie{Name: sessionCookie, Value: au.sign("root", time.Now().Add(time.Hour))}
	try := func(method string, hdr map[string]string, withCookie bool) int {
		r := httptest.NewRequest(method, "https://ipcr-forge-x.example/admin/api/repos", nil)
		r.Host = "ipcr-forge-x.example"
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		if withCookie {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	same := map[string]string{"X-IPCR-Admin": "1", "Origin": "https://ipcr-forge-x.example"}
	for _, c := range []struct {
		name   string
		method string
		hdr    map[string]string
		cookie bool
		want   int
	}{
		{"no session", "GET", nil, false, 401},
		{"read", "GET", nil, true, 200},
		{"write, same origin", "PUT", same, true, 200},
		{"write, no header", "PUT", map[string]string{"Origin": "https://ipcr-forge-x.example"}, true, 403},
		{"write, other origin", "PUT", map[string]string{"X-IPCR-Admin": "1", "Origin": "https://evil.example"}, true, 403},
		{"write, fetch metadata", "POST", map[string]string{"X-IPCR-Admin": "1", "Sec-Fetch-Site": "same-origin"}, true, 200},
	} {
		if got := try(c.method, c.hdr, c.cookie); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}

// The login round trip against a fake Gitea: an administrator gets a session, anyone else a 403.
func TestCallback(t *testing.T) {
	admin := true
	gitea := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/oauth/access_token":
			r.ParseForm()
			if r.Form.Get("code") != "the-code" || r.Form.Get("code_verifier") == "" || r.Form.Get("client_secret") != "s3cret" ||
				r.Form.Get("redirect_uri") != "https://ipcr-forge-x.example/admin/callback" {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"access_token": "tok"})
		case "/api/v1/user":
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(401)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"login": "root", "is_admin": admin})
		}
	}))
	defer gitea.Close()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "oauth-client-id"), []byte("cid"), 0o600)
	os.WriteFile(filepath.Join(dir, "oauth-client-secret"), []byte("s3cret"), 0o600)
	au, _ := newAuth([]string{"ipcr-forge-x.example"}, gitea.URL, dir, filepath.Join(dir, "k"))

	login := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/admin/login", nil)
	r.Host = "ipcr-forge-x.example"
	au.login(login, r)
	loc, _ := url.Parse(login.Header().Get("Location"))
	if login.Code != 302 || loc.Host != "gitea-x.example" || loc.Query().Get("code_challenge_method") != "S256" {
		t.Fatalf("login redirect: %d %s", login.Code, loc)
	}
	state := loc.Query().Get("state")
	oauth := login.Result().Cookies()[0]

	call := func(state string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/admin/callback?code=the-code&state="+state, nil)
		r.Host = "ipcr-forge-x.example"
		r.AddCookie(oauth)
		w := httptest.NewRecorder()
		au.callback(w, r)
		return w
	}
	if w := call("wrong"); w.Code != 400 {
		t.Errorf("wrong state: %d", w.Code)
	}
	w := call(state)
	if w.Code != 302 || w.Header().Get("Location") != "/admin" {
		t.Fatalf("admin callback: %d %s", w.Code, w.Body)
	}
	var session string
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			session = c.Value
		}
	}
	if u, err := au.user(func() *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
		return r
	}()); err != nil || u != "root" {
		t.Errorf("session after login: %q %v", u, err)
	}
	admin = false
	if w := call(state); w.Code != 403 {
		t.Errorf("non-admin: %d", w.Code)
	}

	// Unknown host: no redirect to Gitea.
	r = httptest.NewRequest("GET", "/admin/login", nil)
	r.Host = "evil.example"
	w = httptest.NewRecorder()
	au.login(w, r)
	if w.Code != 400 {
		t.Errorf("unknown host: %d", w.Code)
	}
}

func TestPlan(t *testing.T) {
	mk := func(name string, f func(*repo)) repo {
		r := repo{FullName: name}
		if f != nil {
			f(&r)
		}
		return r
	}
	repos := []repo{
		mk("Owner/App", nil),
		mk("owner/off", nil),
		mk("owner/secret", func(r *repo) { r.Private = true }),
		mk("owner/fork", func(r *repo) { r.Fork = true }),
		mk("Metadec/Hello", nil),
		mk("metadec/owner", nil), // short path /owner is owner/'s folder
		mk("metadec/bob", nil),   // a Gitea user is named bob
		mk("metadec/off2", nil),
	}
	p := plan(repos, []string{"owner/off", "metadec/off2"}, "metadec", func(n string) bool { return n == "bob" })
	if got := strings.Join(p.Repos, ","); got != "metadec/bob,metadec/hello,metadec/owner,owner/app" {
		t.Errorf("repos = %s", got)
	}
	if len(p.Aliases) != 1 || strings.Join(p.Aliases["metadec/hello"], ",") != "hello" {
		t.Errorf("aliases = %v", p.Aliases)
	}
	if p.Collisions["metadec/owner"] == "" || p.Collisions["metadec/bob"] == "" || len(p.Collisions) != 2 {
		t.Errorf("collisions = %v", p.Collisions)
	}
	if q := plan(repos, nil, "", nil); len(q.Aliases) != 0 {
		t.Errorf("no root organisation, aliases %v", q.Aliases)
	}
}
