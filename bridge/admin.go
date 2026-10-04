package main

import (
	"bytes"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// adminAPI is what the admin page calls, behind auth.requireAdmin:
//
//	/admin/api/me                     who is signed in
//	/admin/api/ipcr/<path>            ipcrd's admin API (gateway/admin.go), with its token
//	POST /admin/api/key/export        the publisher key, encrypted with the given passphrase
//	GET  /admin/api/repos             Gitea's public repositories and whether each may publish images
//	PUT  /admin/api/repos             {full_name, publish}
//
// It also keeps ipcrd's allowlist equal to "public Gitea repositories, minus those switched off":
// a build can push any image name to the staging registry, and only those names are published.
type adminAPI struct {
	ipcr      string // ipcrd's admin API, e.g. http://ipcr-gateway:4769
	tokenFile string // ipcrd's admin-token, read through the read-only ipcr/state mount
	keystore  string // Kubo's keystore folder, read-only
	publisher string // the publisher key's name (IMPORT_PUBLISHER)
	settings  string // admin.json: the repositories switched off
	gitea     *gitea
	au        *auth
	http      *http.Client

	mu sync.Mutex
}

type adminSettings struct {
	// Repositories (owner/name, lowercase) whose images are not published.
	Disabled []string `json:"disabled"`
}

func (a *adminAPI) load() adminSettings {
	var s adminSettings
	if b, err := os.ReadFile(a.settings); err == nil {
		json.Unmarshal(b, &s)
	}
	return s
}

func (a *adminAPI) token() string {
	b, _ := os.ReadFile(a.tokenFile)
	return strings.TrimSpace(string(b))
}

func (a *adminAPI) routes(mux *http.ServeMux, au *auth) {
	a.au = au
	guard := au.requireAdmin
	mux.Handle("GET /admin/api/me", guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, _ := au.user(r)
		writeJSONResponse(w, map[string]string{"user": u, "publisher": a.publisher})
	})))
	mux.Handle("/admin/api/ipcr/{path...}", guard(http.HandlerFunc(a.proxy)))
	mux.Handle("POST /admin/api/key/export", guard(http.HandlerFunc(a.export)))
	mux.Handle("GET /admin/api/repos", guard(http.HandlerFunc(a.listRepos)))
	mux.Handle("PUT /admin/api/repos", guard(http.HandlerFunc(a.setRepo)))
}

func writeJSONResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

// proxy forwards to ipcrd's admin API. Only its own paths: /admin/<path>.
func (a *adminAPI) proxy(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("path")
	if strings.Contains(p, "..") {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	u := a.ipcr + "/admin/" + p
	if r.URL.RawQuery != "" {
		u += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, u, io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.token())
	resp, err := a.http.Do(req)
	if err != nil {
		// Not 502: a proxy in front (Cloudflare) replaces a 502's body with its own page.
		http.Error(w, "IPCR unreachable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// keystoreFile is where Kubo keeps the key <name>: "key_" + lowercase unpadded base32 of the name.
func keystoreFile(dir, name string) string {
	return filepath.Join(dir, "key_"+strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte(name))))
}

// export reads the publisher key from Kubo's keystore and returns it sealed with the passphrase.
// The cleartext key exists only in this process's memory, for the length of the request.
func (a *adminAPI) export(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Passphrase string `json:"passphrase"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "{passphrase}?", http.StatusBadRequest)
		return
	}
	key, err := os.ReadFile(keystoreFile(a.keystore, a.publisher))
	if err != nil {
		log.Printf("admin: key export: %v", err)
		http.Error(w, "cannot read the publisher key "+a.publisher+" (has anything been published yet?)", http.StatusNotFound)
		return
	}
	b, err := sealKey(a.publisher, key, req.Passphrase)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u, _ := a.au.user(r)
	log.Printf("admin: publisher key %s (%s) exported by %s", a.publisher, b.ID, u)
	name := fmt.Sprintf("%s-%s-%s.ipcrkey.json", a.publisher, b.ID[len(b.ID)-8:], time.Now().UTC().Format("20060102"))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(b)
}

type repoToggle struct {
	FullName string `json:"full_name"`
	Publish  bool   `json:"publish"`
}

func (a *adminAPI) listRepos(w http.ResponseWriter, r *http.Request) {
	repos, err := a.gitea.publicRepos()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	disabled := map[string]bool{}
	for _, d := range a.load().Disabled {
		disabled[d] = true
	}
	out := []repoToggle{}
	for _, rp := range repos {
		if eligible(&rp) {
			n := strings.ToLower(rp.FullName)
			out = append(out, repoToggle{FullName: n, Publish: !disabled[n]})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FullName < out[j].FullName })
	writeJSONResponse(w, out)
}

func (a *adminAPI) setRepo(w http.ResponseWriter, r *http.Request) {
	var req repoToggle
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil || req.FullName == "" {
		http.Error(w, "{full_name, publish}?", http.StatusBadRequest)
		return
	}
	n := strings.ToLower(req.FullName)
	a.mu.Lock()
	s := a.load()
	var keep []string
	for _, d := range s.Disabled {
		if d != n {
			keep = append(keep, d)
		}
	}
	if !req.Publish {
		keep = append(keep, n)
	}
	sort.Strings(keep)
	s.Disabled = keep
	b, _ := json.MarshalIndent(s, "", "  ")
	err := os.WriteFile(a.settings+".new", b, 0o644)
	if err == nil {
		err = os.Rename(a.settings+".new", a.settings)
	}
	a.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := a.pushAllowlist(); err != nil {
		http.Error(w, "saved, but IPCR did not take the new list: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	writeJSONResponse(w, req)
}

// eligible: the repositories whose images may be published — the same that are mirrored.
func eligible(r *repo) bool { return r.public() && !r.Fork && !r.Mirror }

// allowlist is what ipcrd publishes: the image names CI builds are pushed under
// (localhost:5000/<owner>/<repo>, lowercased by docker/metadata-action).
func allowlist(repos []repo, disabled []string) []string {
	off := map[string]bool{}
	for _, d := range disabled {
		off[d] = true
	}
	var out []string
	for i := range repos {
		n := strings.ToLower(repos[i].FullName)
		if eligible(&repos[i]) && !off[n] {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// pushAllowlist sends ipcrd the current list. Called on every reconcile and on every toggle.
func (a *adminAPI) pushAllowlist() error {
	repos, err := a.gitea.publicRepos()
	if err != nil {
		return err
	}
	return a.sendAllowlist(allowlist(repos, a.load().Disabled))
}

func (a *adminAPI) sendAllowlist(list []string) error {
	if list == nil {
		list = []string{}
	}
	body, _ := json.Marshal(map[string]any{"allow": map[string]any{"required": true, "repos": list}})
	req, _ := http.NewRequest("PATCH", a.ipcr+"/admin/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.token())
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return errors.New(resp.Status + " " + strings.TrimSpace(string(b)))
	}
	return nil
}
