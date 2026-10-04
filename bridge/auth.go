package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Admin login: Gitea is the forge's one identity system, so the admin pages sign in with it
// (OAuth2 authorization code + PKCE, an app the setup step registers) and let in Gitea site admins
// only. The Gitea token is used once, to read who signed in, and dropped; what remains is this
// service's own session cookie, HMAC-signed with a key kept in STATE_DIR.
//
// The browser goes to Gitea's public address, matching the host the page was opened on
// (ipcr-forge-<suffix> → gitea-<suffix>); the code is exchanged over the internal one.
type auth struct {
	hosts      map[string]bool // ipcr-forge-… hosts the page is served on (PUBLIC_HOSTS)
	gitea      string          // internal address, for the code exchange and /api/v1/user
	secretsDir string          // oauth-client-id, oauth-client-secret (written by setup)
	key        []byte          // session signing key
	http       *http.Client
}

const (
	sessionCookie = "ipcr_admin"
	oauthCookie   = "ipcr_oauth"
	sessionTTL    = 8 * time.Hour
)

func newAuth(hosts []string, gitea, secretsDir, keyFile string) (*auth, error) {
	key, err := loadOrCreateKey(keyFile)
	if err != nil {
		return nil, err
	}
	a := &auth{hosts: map[string]bool{}, gitea: strings.TrimRight(gitea, "/"), secretsDir: secretsDir, key: key,
		http: &http.Client{Timeout: 30 * time.Second}}
	for _, h := range hosts {
		if h = strings.TrimSpace(h); h != "" {
			a.hosts[h] = true
		}
	}
	return a, nil
}

// loadOrCreateKey keeps the session key across restarts, so a restart does not sign everyone out.
func loadOrCreateKey(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		return b, nil
	}
	b := make([]byte, 32)
	rand.Read(b)
	return b, writeSecret(path, string(b))
}

func (a *auth) secret(name string) string {
	b, _ := os.ReadFile(a.secretsDir + "/" + name)
	return strings.TrimSpace(string(b))
}

// configured reports whether the setup step has registered the OAuth app yet.
func (a *auth) configured() bool {
	return a.secret("oauth-client-id") != "" && a.secret("oauth-client-secret") != ""
}

func randomString(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// giteaPublic is Gitea's address for a browser on host: same suffix, gitea- instead of ipcr-forge-.
func giteaPublic(host string) string {
	return "https://gitea-" + strings.TrimPrefix(host, "ipcr-forge-")
}

func (a *auth) login(w http.ResponseWriter, r *http.Request) {
	if !a.hosts[r.Host] {
		http.Error(w, "unknown host "+r.Host, http.StatusBadRequest)
		return
	}
	if !a.configured() {
		http.Error(w, "admin login is not set up yet: the forge-setup install step registers it on the next start", http.StatusServiceUnavailable)
		return
	}
	state, verifier := randomString(16), randomString(32)
	sum := sha256.Sum256([]byte(verifier))
	http.SetCookie(w, &http.Cookie{Name: oauthCookie, Value: state + "." + verifier, Path: "/admin",
		MaxAge: 600, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	q := url.Values{
		"client_id": {a.secret("oauth-client-id")}, "redirect_uri": {"https://" + r.Host + "/admin/callback"},
		"response_type": {"code"}, "state": {state}, "scope": {"read:user"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
	}
	http.Redirect(w, r, giteaPublic(r.Host)+"/login/oauth/authorize?"+q.Encode(), http.StatusFound)
}

func (a *auth) callback(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(oauthCookie)
	state, verifier, _ := strings.Cut(func() string {
		if err != nil {
			return ""
		}
		return c.Value
	}(), ".")
	if state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(r.FormValue("state"))) != 1 || !a.hosts[r.Host] {
		http.Error(w, "login expired or not started here: open /admin again", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: oauthCookie, Path: "/admin", MaxAge: -1})
	if e := r.FormValue("error"); e != "" {
		http.Error(w, "Gitea refused: "+e, http.StatusForbidden)
		return
	}
	user, admin, err := a.exchange(r.FormValue("code"), verifier, "https://"+r.Host+"/admin/callback")
	if err != nil {
		log.Printf("admin login: %v", err)
		http.Error(w, "login failed", http.StatusInternalServerError)
		return
	}
	if !admin {
		log.Printf("admin login: %s is not a Gitea administrator", user)
		http.Error(w, user+" is not a Gitea administrator. Only Gitea site administrators can use these pages.", http.StatusForbidden)
		return
	}
	log.Printf("admin login: %s", user)
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: a.sign(user, time.Now().Add(sessionTTL)), Path: "/",
		MaxAge: int(sessionTTL.Seconds()), HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/admin", http.StatusFound)
}

// exchange trades the code for a token, then asks Gitea who that is. The token is not kept.
func (a *auth) exchange(code, verifier, redirect string) (string, bool, error) {
	resp, err := a.http.PostForm(a.gitea+"/login/oauth/access_token", url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect}, "code_verifier": {verifier},
		"client_id": {a.secret("oauth-client-id")}, "client_secret": {a.secret("oauth-client-secret")},
	})
	if err != nil {
		return "", false, err
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&tok)
	resp.Body.Close()
	if tok.AccessToken == "" {
		return "", false, fmt.Errorf("token exchange: %s %s", resp.Status, tok.Error)
	}
	req, _ := http.NewRequest("GET", a.gitea+"/api/v1/user", nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	resp, err = a.http.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	var u struct {
		Login   string `json:"login"`
		IsAdmin bool   `json:"is_admin"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&u) != nil || u.Login == "" {
		return "", false, fmt.Errorf("/api/v1/user: %s", resp.Status)
	}
	return u.Login, u.IsAdmin, nil
}

func (a *auth) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// sign makes a session value: base64url(user|expiry).base64url(HMAC).
func (a *auth) sign(user string, exp time.Time) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(user + "|" + fmt.Sprint(exp.Unix())))
	m := hmac.New(sha256.New, a.key)
	m.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

var errNoSession = errors.New("not signed in")

// user returns who the session cookie belongs to, if it is valid and current.
func (a *auth) user(r *http.Request) (string, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", errNoSession
	}
	payload, sig, _ := strings.Cut(c.Value, ".")
	m := hmac.New(sha256.New, a.key)
	m.Write([]byte(payload))
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, m.Sum(nil)) {
		return "", errNoSession
	}
	raw, _ := base64.RawURLEncoding.DecodeString(payload)
	user, exp, _ := strings.Cut(string(raw), "|")
	var unix int64
	if _, err := fmt.Sscan(exp, &unix); err != nil || time.Now().Unix() > unix || user == "" {
		return "", errNoSession
	}
	return user, nil
}

// requireAdmin guards the admin API: a valid session, and for anything that changes state, a
// same-origin request carrying X-IPCR-Admin (a header a cross-site form cannot send, and a
// cross-site script cannot without a CORS preflight this service never answers).
func (a *auth) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := a.user(r); err != nil {
			http.Error(w, "sign in at /admin", http.StatusUnauthorized)
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" && !sameOrigin(r) {
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func sameOrigin(r *http.Request) bool {
	if r.Header.Get("X-IPCR-Admin") != "1" {
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		return o == "https://"+r.Host
	}
	return r.Header.Get("Sec-Fetch-Site") == "same-origin"
}
