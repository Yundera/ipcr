package main

import (
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

//go:embed all:example
var exampleFiles embed.FS

const (
	tokenName      = "ipcr-forge-bridge"
	adminTokenName = "ipcr-forge-secrets"
)

// ensureOrg creates the public organisation org, owned by the admin, unless it exists.
func ensureOrg(g *gitea, org string) error {
	code, err := g.call("GET", "/orgs/"+org, nil, nil)
	if code == http.StatusOK {
		return nil
	}
	if code != http.StatusNotFound {
		return err
	}
	if _, err := g.call("POST", "/admin/users/"+g.owner+"/orgs", map[string]any{
		"username": org, "visibility": "public",
		"description": "Its public repositories publish images at the root of the forge's name.",
	}, nil); err != nil {
		return err
	}
	log.Printf("setup: created organisation %s", org)
	return nil
}

// setup is the install step, run after the stack is up and on every start (each action is
// idempotent). It signs in once as the admin with the server's default app password, because
// Gitea's token API accepts nothing else, and leaves:
//
//	SECRETS_DIR/gitea-token  public-only + read:repository: the daemon lists and clones public
//	                         repositories, and cannot see any other. Minted only when missing.
//	SECRETS_DIR/hook-secret  the webhook's HMAC key. Generated only when missing.
//	a system webhook         every repository's push/create/delete/repository events → HOOK_URL,
//	                         created or repaired (matched by URL), never duplicated.
//	SECRETS_DIR/gitea-admin-token  write:repository + read:user: push secrets, name collisions.
//	IPCR_ROOT_ORG            the root organisation, created when absent (public, owned by the admin).
//	EXAMPLE_REPO             a public example repository with a workflow, created when absent, in
//	                         the root organisation when there is one.
//	an OAuth2 application    the admin pages' Gitea login (PUBLIC_HOSTS → its redirect URIs);
//	                         SECRETS_DIR/oauth-client-id and oauth-client-secret.
//
// The admin password is used here only and never stored. If it was changed since install, the
// step leaves the existing token and hook alone instead of failing the start.
func setup() error {
	secrets := env("SECRETS_DIR", "/secrets")
	g := newGitea(env("GITEA_URL", "http://gitea:3000"), env("GITEA_OWNER", "gitea_admin"), filepath.Join(secrets, "gitea-token"))
	g.password = os.Getenv("GITEA_PASSWORD")
	if g.password == "" {
		return errors.New("GITEA_PASSWORD is required")
	}

	// Gitea may still be migrating, and its admin account may not exist yet. Bounded: an install
	// step that blocks forever holds the install open forever.
	var code int
	var err error
	for i := 0; i < 80; i++ {
		if code, err = g.call("GET", "/user", nil, nil); err == nil {
			break
		}
		if code == http.StatusUnauthorized && g.hasToken() {
			log.Printf("setup: the admin password no longer works; keeping the existing token and webhook")
			return nil
		}
		time.Sleep(3 * time.Second)
	}
	if err != nil {
		return fmt.Errorf("Gitea not ready after 240s: %w", err)
	}

	if !g.hasToken() {
		g.call("DELETE", "/users/"+g.owner+"/tokens/"+tokenName, nil, nil)
		var t struct {
			SHA1 string `json:"sha1"`
		}
		if _, err := g.call("POST", "/users/"+g.owner+"/tokens", map[string]any{
			"name": tokenName, "scopes": []string{"public-only", "read:repository"},
		}, &t); err != nil {
			return fmt.Errorf("token: %w", err)
		}
		if err := writeSecret(g.tokenFile, t.SHA1); err != nil {
			return err
		}
		log.Printf("setup: token %s written", tokenName)
	}

	// The admin-scoped token: sets each publishing repository's IPCR_PUSH_TOKEN secret, and checks
	// short-path collisions against every user and organisation. Not public-only: an organisation
	// may be private and still own the name.
	adminTokenFile := filepath.Join(secrets, "gitea-admin-token")
	if b, _ := os.ReadFile(adminTokenFile); len(strings.TrimSpace(string(b))) == 0 {
		g.call("DELETE", "/users/"+g.owner+"/tokens/"+adminTokenName, nil, nil)
		var t struct {
			SHA1 string `json:"sha1"`
		}
		if _, err := g.call("POST", "/users/"+g.owner+"/tokens", map[string]any{
			"name": adminTokenName, "scopes": []string{"write:repository", "read:user", "read:organization"},
		}, &t); err != nil {
			return fmt.Errorf("token: %w", err)
		}
		if err := writeSecret(adminTokenFile, t.SHA1); err != nil {
			return err
		}
		log.Printf("setup: token %s written", adminTokenName)
	}

	// The root organisation: its repositories are also published at the root of the forge's name.
	owner := g.owner
	if org := strings.ToLower(env("IPCR_ROOT_ORG", "")); org != "" {
		if err := ensureOrg(g, org); err != nil {
			log.Printf("setup: organisation %s: %v", org, err)
		} else {
			owner = org
		}
	}

	secretFile := filepath.Join(secrets, "hook-secret")
	secret, _ := os.ReadFile(secretFile)
	if strings.TrimSpace(string(secret)) == "" {
		b := make([]byte, 32)
		rand.Read(b)
		secret = []byte(hex.EncodeToString(b))
		if err := writeSecret(secretFile, string(secret)); err != nil {
			return err
		}
	}
	if err := ensureHook(g, env("HOOK_URL", "http://ipcr-forge:8081/hooks/gitea"), strings.TrimSpace(string(secret))); err != nil {
		return fmt.Errorf("webhook: %w", err)
	}

	if hosts := splitList(env("PUBLIC_HOSTS", "")); len(hosts) > 0 {
		if err := ensureOAuthApp(g, hosts, secrets); err != nil {
			return fmt.Errorf("admin login (OAuth2 app): %w", err)
		}
	}

	if name := env("EXAMPLE_REPO", "ipcr-hello"); name != "-" && name != "" {
		// Not fatal: the forge works without its example.
		if err := ensureExample(g, owner, name); err != nil {
			log.Printf("setup: example repository: %v", err)
		}
	}
	return nil
}

func writeSecret(path, value string) error {
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(value), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ensureHook creates the system webhook, or rewrites the one already pointing at url (Gitea never
// returns a hook's secret, so it is set every time rather than compared).
func ensureHook(g *gitea, url, secret string) error {
	config := map[string]string{"url": url, "content_type": "json", "secret": secret, "is_system_webhook": "true"}
	events := []string{"push", "create", "delete", "repository"}
	var hooks []struct {
		ID     int64             `json:"id"`
		Config map[string]string `json:"config"`
	}
	if _, err := g.call("GET", "/admin/hooks?type=all&limit=50", nil, &hooks); err != nil {
		return err
	}
	for _, h := range hooks {
		if h.Config["url"] == url {
			_, err := g.call("PATCH", fmt.Sprintf("/admin/hooks/%d", h.ID), map[string]any{
				"config": config, "events": events, "active": true, "branch_filter": "*",
			}, nil)
			return err
		}
	}
	_, err := g.call("POST", "/admin/hooks", map[string]any{
		"type": "gitea", "config": config, "events": events, "active": true, "branch_filter": "*",
	}, nil)
	if err == nil {
		log.Printf("setup: system webhook → %s", url)
	}
	return err
}

// ensureExample creates <owner>/<name>, public, with the files under example/ in one commit. A
// repository that already has content is left as it is: it may be the user's by now.
func ensureExample(g *gitea, owner, name string) error {
	var r repo
	code, err := g.call("GET", "/repos/"+owner+"/"+name, nil, &r)
	switch {
	case code == http.StatusNotFound:
		create := "/admin/users/" + owner + "/repos"
		if owner != g.owner {
			create = "/orgs/" + owner + "/repos"
		}
		if _, err := g.call("POST", create, map[string]any{
			"name": name, "private": false, "default_branch": "main",
			"description": "Example: a tag here builds an image, published on IPFS by IPCR and mirrored to Radicle.",
		}, &r); err != nil {
			return err
		}
		log.Printf("setup: created %s/%s", owner, name)
	case err != nil:
		return err
	case !r.Empty:
		return nil
	}
	var files []map[string]string
	err = fs.WalkDir(exampleFiles, "example", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := exampleFiles.ReadFile(p)
		if err != nil {
			return err
		}
		files = append(files, map[string]string{
			"operation": "create", "path": strings.TrimPrefix(p, "example/"),
			"content": base64.StdEncoding.EncodeToString(b),
		})
		return nil
	})
	if err != nil {
		return err
	}
	_, err = g.call("POST", "/repos/"+owner+"/"+name+"/contents", map[string]any{
		"branch": "main", "message": "Example: a Dockerfile and the workflow that publishes it", "files": files,
	}, nil)
	return err
}

const oauthAppName = "ipcr-forge-admin"

func splitList(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		out = append(out, f)
	}
	return out
}

// ensureOAuthApp registers the admin pages as a Gitea OAuth2 application, one redirect URI per host
// the page is served on. Gitea shows a client secret only when the app is created, so a missing
// secret file means re-creating the app.
func ensureOAuthApp(g *gitea, hosts []string, secrets string) error {
	var redirects []string
	for _, h := range hosts {
		redirects = append(redirects, "https://"+h+"/admin/callback")
	}
	body := map[string]any{"name": oauthAppName, "redirect_uris": redirects,
		"confidential_client": true, "skip_secondary_authorization": true}
	var apps []struct {
		ID           int64    `json:"id"`
		Name         string   `json:"name"`
		ClientID     string   `json:"client_id"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if _, err := g.call("GET", "/user/applications/oauth2?limit=50", nil, &apps); err != nil {
		return err
	}
	idFile, secretFile := filepath.Join(secrets, "oauth-client-id"), filepath.Join(secrets, "oauth-client-secret")
	haveSecret := false
	if b, err := os.ReadFile(secretFile); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		haveSecret = true
	}
	for _, app := range apps {
		if app.Name != oauthAppName {
			continue
		}
		if haveSecret {
			// Keep it, and its secret. Gitea regenerates the secret on every update, so only
			// update when the hosts changed, and keep the secret it hands back.
			if strings.Join(app.RedirectURIs, ",") == strings.Join(redirects, ",") {
				return nil
			}
			var updated struct {
				ClientSecret string `json:"client_secret"`
			}
			if _, err := g.call("PATCH", fmt.Sprintf("/user/applications/oauth2/%d", app.ID), body, &updated); err != nil {
				return err
			}
			if updated.ClientSecret != "" {
				if err := writeSecret(secretFile, updated.ClientSecret); err != nil {
					return err
				}
			}
			log.Printf("setup: OAuth2 app %s now for %s", oauthAppName, strings.Join(redirects, ", "))
			return nil
		}
		if _, err := g.call("DELETE", fmt.Sprintf("/user/applications/oauth2/%d", app.ID), nil, nil); err != nil {
			return err
		}
	}
	var created struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if _, err := g.call("POST", "/user/applications/oauth2", body, &created); err != nil {
		return err
	}
	if created.ClientID == "" || created.ClientSecret == "" {
		return errors.New("Gitea returned no client credentials")
	}
	if err := writeSecret(idFile, created.ClientID); err != nil {
		return err
	}
	if err := writeSecret(secretFile, created.ClientSecret); err != nil {
		return err
	}
	log.Printf("setup: OAuth2 app %s for %s", oauthAppName, strings.Join(redirects, ", "))
	return nil
}
