// ipcr-forge-bridge — the glue of IPCR Forge: Gitea is where people work, Radicle is where the code
// is replicated, and this service keeps the second following the first. One way only.
//
//	serve (default)  webhook receiver + mirror worker + the forge's page
//	setup            install step: Gitea token, system webhook, example repository (idempotent)
//	health           exit 0 when `serve` answers, for the container healthcheck
//
// What gets mirrored: every public, non-fork, non-mirror Gitea repository, as seen by a token
// scoped `public-only` — a private repository is invisible to the service, not filtered by it.
// Each one is `rad init`ed by this node on first sight (the node's key is its only delegate) and
// then receives its branches and tags with `git push rad`. A repository that turns private or is
// deleted is frozen: no further sync, and nothing is taken back from the network (it cannot be).
//
// Triggers: Gitea's system webhook (push, create, delete, repository) for latency, and a full
// reconcile every RECONCILE for whatever a lost delivery, a rename or a visibility change missed.
//
// Environment:
//
//	GITEA_URL        Gitea, internal address (default http://gitea:3000)
//	GITEA_OWNER      the admin account (default gitea_admin)
//	GITEA_PASSWORD   setup only: its password, used once to mint the token and the hook
//	SECRETS_DIR      gitea-token and hook-secret (default /secrets)
//	STATE_DIR        repos.json and the bare mirrors (default /bridge)
//	RAD_HOME         the node's Radicle home (default /radicle-home)
//	WEB_LISTEN       the page (default :8080)
//	HOOK_LISTEN      the webhook, internal only (default :8081)
//	HOOK_URL         setup only: how Gitea reaches HOOK_LISTEN (default http://ipcr-forge:8081/hooks/gitea)
//	EXAMPLE_REPO     setup only: example repository to create under GITEA_OWNER; empty = none
//	                 (default ipcr-hello)
//	IPCR_PUBLISHED   IPCR's published.json, served as /images.json
//	                 (default /srv/ipcr-state/published.json)
//	RECONCILE        full pass interval (default 10m)
//
// Admin pages (/admin, auth.go and admin.go), on when IPCR_ADMIN is set:
//
//	PUBLIC_HOSTS           the hosts the page is served on (ipcr-forge-<domain>, …), comma separated;
//	                       setup registers one OAuth2 redirect URI per host
//	IPCR_ADMIN             ipcrd's admin API (e.g. http://ipcr-gateway:4769)
//	IPCR_ADMIN_TOKEN_FILE  its token (default /srv/ipcr-state/admin-token)
//	KUBO_KEYSTORE          Kubo's keystore, read-only, for key backups (default /srv/ipcr-keystore)
//	IMPORT_PUBLISHER       the publisher key's name (default forge)
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "setup":
		err = setup()
	case "health":
		err = health()
	default:
		err = fmt.Errorf("usage: %s serve | setup | health", os.Args[0])
	}
	if err != nil {
		log.Fatal(err)
	}
}

func health() error {
	c := &http.Client{Timeout: 5 * time.Second}
	r, err := c.Get("http://127.0.0.1" + env("WEB_LISTEN", ":8080") + "/healthz")
	if err != nil {
		return err
	}
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz: %s", r.Status)
	}
	return nil
}

func serve() error {
	every, err := time.ParseDuration(env("RECONCILE", "10m"))
	if err != nil {
		return fmt.Errorf("RECONCILE: %w", err)
	}
	stateDir := env("STATE_DIR", "/bridge")
	st, err := loadState(stateDir + "/repos.json")
	if err != nil {
		return err
	}
	secrets := env("SECRETS_DIR", "/secrets")
	m := &mirror{
		gitea:   newGitea(env("GITEA_URL", "http://gitea:3000"), env("GITEA_OWNER", "gitea_admin"), secrets+"/gitea-token"),
		state:   st,
		repoDir: stateDir + "/repos",
		radHome: env("RAD_HOME", "/radicle-home"),
	}
	if err := os.MkdirAll(m.repoDir, 0o755); err != nil {
		return err
	}
	q := newQueue()
	go q.run(m.sync)

	hook := &hookHandler{secretFile: secrets + "/hook-secret", enqueue: q.add}
	hmux := http.NewServeMux()
	hmux.Handle("POST /hooks/gitea", hook)
	go func() {
		log.Fatal(http.ListenAndServe(env("HOOK_LISTEN", ":8081"), hmux))
	}()

	mux := webHandler(st, env("IPCR_PUBLISHED", "/srv/ipcr-state/published.json"))
	if ipcr := os.Getenv("IPCR_ADMIN"); ipcr != "" {
		au, err := newAuth(splitList(env("PUBLIC_HOSTS", "")), m.gitea.base, secrets, stateDir+"/session-key")
		if err != nil {
			return err
		}
		adm := &adminAPI{
			ipcr: strings.TrimRight(ipcr, "/"), tokenFile: env("IPCR_ADMIN_TOKEN_FILE", "/srv/ipcr-state/admin-token"),
			keystore: env("KUBO_KEYSTORE", "/srv/ipcr-keystore"), publisher: env("IMPORT_PUBLISHER", "forge"),
			settings: stateDir + "/admin.json", gitea: m.gitea, http: &http.Client{Timeout: 3 * time.Minute},
		}
		adminRoutes(mux, au, adm)
		// IPCR publishes nothing until it has the list (IMPORT_ALLOW_REQUIRED): send it as soon as
		// both ends are up, then after every reconcile.
		go func() {
			for {
				if !m.gitea.hasToken() {
					time.Sleep(15 * time.Second)
					continue
				}
				err := adm.pushAllowlist()
				if err == nil {
					return
				}
				log.Printf("allowlist: %v (retrying)", err)
				time.Sleep(30 * time.Second)
			}
		}()
		m.onReconcile = func() {
			if err := adm.pushAllowlist(); err != nil {
				log.Printf("allowlist: %v", err)
			}
		}
		log.Printf("bridge: admin pages on, IPCR admin API %s", ipcr)
	}
	go reconcileLoop(m, q, every)

	log.Printf("bridge: %s → Radicle (%s), reconcile every %s", m.gitea.base, m.radHome, every)
	return http.ListenAndServe(env("WEB_LISTEN", ":8080"), mux)
}

// reconcileLoop queues every repository Gitea shows as public, plus every one already known (so one
// that turned private or was deleted is noticed and frozen). The first pass runs at start-up.
func reconcileLoop(m *mirror, q *queue, every time.Duration) {
	for {
		if !m.gitea.hasToken() {
			// The setup step runs after the stack is up, so on a first install this is normal.
			log.Printf("reconcile: waiting for the Gitea token (written by the setup step)")
			time.Sleep(15 * time.Second)
			continue
		}
		repos, err := m.gitea.publicRepos()
		if err != nil {
			// Gitea restarting, typically: try again soon rather than in a full interval.
			log.Printf("reconcile: %v", err)
			time.Sleep(time.Minute)
			continue
		}
		seen := map[int64]bool{}
		for _, r := range repos {
			seen[r.ID] = true
			q.add(r.ID)
		}
		for _, id := range m.state.ids() {
			if !seen[id] {
				q.add(id)
			}
		}
		if m.onReconcile != nil {
			m.onReconcile()
		}
		time.Sleep(every)
	}
}
