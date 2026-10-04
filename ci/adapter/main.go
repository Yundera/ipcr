// rad-actions — a Radicle CI broker (cib) adapter that runs a repository's GitHub-style workflows
// with act (https://github.com/nektos/act).
//
// cib writes one trigger message (JSON, one line) to stdin; we answer on stdout with exactly two
// lines, "triggered" then "finished". Everything else goes to stderr (captured by cib) or to the
// run's log file. Protocol: radicle-ci-broker doc/adapter-impl.md.
//
//	Radicle event          act event       GITHUB_REF
//	tag pushed             push            refs/tags/<tag>   (cib 0.32 sends these as event_type
//	                                                          "push" with the tag in `branch`)
//	branch pushed          push            refs/heads/<branch>
//	patch opened/updated   pull_request    (act's own: refs/pull/…), base = default branch
//
// The commit is fetched straight from the node's storage, alone (depth 1, as actions/checkout
// does by default): no network, and a self-contained .git that act can copy into the job. Workflows are read from .github/workflows/, else .gitea/ or .forgejo/workflows/, and
// filtered with GitHub's `on:` rules (filter.go) because act does not apply them.
//
// Environment:
//
//	RAD_HOME                 the node's Radicle home (storage/ is read)
//	RAD_ACTIONS_LOG_DIR      where <run>.log is written (served to people)
//	RAD_ACTIONS_LOG_URL      URL prefix of that folder, for cib's report pages (default /ci/logs/)
//	RAD_ACTIONS_WORK_DIR     scratch space for checkouts (default os.TempDir)
//	RAD_ACTIONS_CACHE_DIR    act's action cache (default ~/.cache/act)
//	RAD_ACTIONS_PLATFORMS    act -P mappings, comma separated
//	RAD_ACTIONS_VARS         extra `vars` for workflows, comma separated k=v; IMAGE_PREFIX is special:
//	                         IMAGE = IMAGE_PREFIX/<repository name>
//	RAD_ACTIONS_API_LISTEN   where the GitHub API stand-in listens during a run (default :8099)
//	RAD_ACTIONS_API_URL      how jobs reach it (GITHUB_API_URL). Default: this container's own IP
//	                         address — jobs run on the daemon's host network, where the compose
//	                         network's names do not resolve.
//	RAD_ACTIONS_REPO_URL     the repository's web page; {rid} is replaced (html_url in the API)
//
// The GitHub API stand-in (api.go): actions such as docker/metadata-action read the repository's
// metadata from the REST API. There is no GitHub here, so for the length of a run the adapter
// answers GET /repos/rad/<name> itself, from what the node knows; every other path is a 404.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type request struct {
	Request    string `json:"request"`
	EventType  string `json:"event_type"` // push | tag | patch
	Repository struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		Description   string `json:"description"`
		Private       bool   `json:"private"`
		DefaultBranch string `json:"default_branch"`
	} `json:"repository"`
	Pusher *struct {
		Alias string `json:"alias"`
	} `json:"pusher"`
	Before  string   `json:"before"`
	After   string   `json:"after"`
	Branch  string   `json:"branch"` // the tag name for event_type tag
	Commits []string `json:"commits"`
	Patch   *struct {
		ID     string `json:"id"`
		Title  string `json:"title"`
		Author struct {
			Alias string `json:"alias"`
		} `json:"author"`
		Commits []string `json:"commits"`
	} `json:"patch"`
}

func respond(v map[string]any) {
	b, _ := json.Marshal(v)
	os.Stdout.Write(append(b, '\n'))
}

func finish(ok bool) {
	result := "failure"
	if ok {
		result = "success"
	}
	respond(map[string]any{"response": "finished", "result": result})
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

var unsafeName = regexp.MustCompile(`[^a-z0-9._-]+`)

func main() {
	log.SetFlags(0)
	log.SetPrefix("rad-actions: ")
	line, err := bufio.NewReader(os.Stdin).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		log.Fatalf("read trigger: %v", err)
	}
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		log.Fatalf("parse trigger: %v", err)
	}

	name := strings.Trim(unsafeName.ReplaceAllString(strings.ToLower(req.Repository.Name), "-"), "-.")
	if name == "" {
		name = "repo"
	}
	storage := filepath.Join(env("RAD_HOME", "/radicle-home"), "storage", strings.TrimPrefix(req.Repository.ID, "rad:"))
	var ev event
	var sha, actor string
	switch req.EventType {
	case "tag":
		ev = event{"push", "refs/tags/" + req.Branch}
	case "push":
		ev = event{"push", "refs/heads/" + req.Branch}
		if isTag(storage, req.Branch) {
			ev.Ref = "refs/tags/" + req.Branch
		}
	case "patch":
		ev = event{"pull_request", req.Repository.DefaultBranch}
	default:
		log.Fatalf("unknown event type %q", req.EventType)
	}
	if req.Pusher != nil {
		actor = req.Pusher.Alias
	}
	commits := req.Commits
	if req.Patch != nil {
		commits = req.Patch.Commits
		actor = req.Patch.Author.Alias
	}
	if len(commits) > 0 {
		sha = commits[0]
	} else {
		sha = req.After
	}
	if actor == "" {
		actor = "radicle"
	}

	runID := time.Now().UTC().Format("20060102T150405Z") + "-" + name + "-" + short(sha)
	logDir := env("RAD_ACTIONS_LOG_DIR", os.TempDir())
	logFile, err := os.Create(filepath.Join(logDir, runID+".log"))
	if err != nil {
		log.Fatalf("log file: %v", err)
	}
	defer logFile.Close()
	out := io.MultiWriter(logFile, os.Stderr)
	say := func(format string, a ...any) { fmt.Fprintf(out, "▶ "+format+"\n", a...) }

	respond(map[string]any{
		"response": "triggered",
		"run_id":   map[string]string{"id": runID},
		"info_url": env("RAD_ACTIONS_LOG_URL", "/ci/logs/") + runID + ".log",
	})
	say("%s %s of %s (%s) at %s", ev.Name, ev.Ref, req.Repository.Name, req.Repository.ID, sha)

	// The node's logs are public pages; a private repository's would not be.
	if req.Repository.Private {
		say("private repository: not run")
		finish(true)
		return
	}

	work, err := os.MkdirTemp(env("RAD_ACTIONS_WORK_DIR", ""), "run-")
	if err != nil {
		say("workspace: %v", err)
		finish(false)
		return
	}
	defer os.RemoveAll(work)
	src := filepath.Join(work, name)
	git := func(args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Stderr = out
		b, err := cmd.Output()
		return strings.TrimSpace(string(b)), err
	}
	// Storage only advertises refs, so the fetch by object ID needs allowAnySHA1InWant on the
	// serving side, which for a local path is this upload-pack. A tag event's tip is the tag
	// object; ^{commit} peels it.
	_, err = git("init", "--quiet", src)
	if err == nil {
		_, err = git("-C", src, "fetch", "--quiet", "--depth=1", "--no-tags",
			"--upload-pack=git -c uploadpack.allowAnySHA1InWant=true upload-pack", "file://"+storage, sha)
	}
	if err != nil {
		say("fetch %s: %v", sha, err)
		finish(false)
		return
	}
	commit, err := git("-C", src, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if err == nil {
		_, err = git("-C", src, "-c", "advice.detachedHead=false", "checkout", "--quiet", commit)
	}
	if err != nil {
		say("checkout %s: %v", sha, err)
		finish(false)
		return
	}
	if ev.Name == "push" && strings.HasPrefix(ev.Ref, "refs/heads/") {
		// act reads the branch from the checkout.
		git("-C", src, "checkout", "--quiet", "-B", req.Branch)
	}

	workflows := pickWorkflows(src, ev, say)
	if len(workflows) == 0 {
		say("no workflow runs on this event")
		finish(true)
		return
	}

	payload := map[string]any{
		"repository": map[string]any{
			"name": name, "full_name": "rad/" + name, "default_branch": req.Repository.DefaultBranch,
			"owner": map[string]any{"login": "rad"},
		},
		"sender": map[string]any{"login": actor},
	}
	if ev.Name == "push" {
		payload["ref"] = ev.Ref
		payload["before"] = req.Before
		payload["after"] = commit
		payload["head_commit"] = map[string]any{"id": commit}
	} else {
		payload["action"] = "synchronize"
		payload["pull_request"] = map[string]any{
			"title": req.Patch.Title,
			"head":  map[string]any{"ref": "patches/" + req.Patch.ID, "sha": commit},
			"base":  map[string]any{"ref": req.Repository.DefaultBranch},
		}
	}
	eventPath := filepath.Join(work, "event.json")
	b, _ := json.Marshal(payload)
	if err := os.WriteFile(eventPath, b, 0o600); err != nil {
		say("event: %v", err)
		finish(false)
		return
	}

	stopAPI, err := serveAPI(env("RAD_ACTIONS_API_LISTEN", ":8099"), name, req.Repository.Description,
		req.Repository.DefaultBranch, strings.ReplaceAll(os.Getenv("RAD_ACTIONS_REPO_URL"), "{rid}", req.Repository.ID))
	if err != nil {
		say("GitHub API stand-in: %v", err)
		finish(false)
		return
	}
	defer stopAPI()

	args := []string{ev.Name, "--eventpath", eventPath, "--pull=false", "--rm",
		"--env", "GITHUB_API_URL=" + env("RAD_ACTIONS_API_URL", "http://"+ownIP()+env("RAD_ACTIONS_API_LISTEN", ":8099")),
		"--actor", actor, "--defaultbranch", req.Repository.DefaultBranch,
		"--env", "GITHUB_REPOSITORY=rad/" + name, "--env", "GITHUB_REPOSITORY_OWNER=rad",
		"--env", "RADICLE_RID=" + req.Repository.ID,
		"--var", "RADICLE_RID=" + req.Repository.ID,
	}
	if d := os.Getenv("RAD_ACTIONS_CACHE_DIR"); d != "" {
		args = append(args, "--action-cache-path", d)
	}
	for _, p := range list(os.Getenv("RAD_ACTIONS_PLATFORMS")) {
		args = append(args, "-P", p)
	}
	for _, kv := range list(os.Getenv("RAD_ACTIONS_VARS")) {
		args = append(args, "--var", kv)
		if v, ok := strings.CutPrefix(kv, "IMAGE_PREFIX="); ok {
			args = append(args, "--var", "IMAGE="+v+"/"+name)
		}
	}

	ok := true
	for _, wf := range workflows {
		say("workflow %s", wf)
		cmd := exec.Command("act", append(args, "--workflows", filepath.Join(src, wf))...)
		cmd.Dir = src
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Run(); err != nil {
			say("workflow %s failed: %v", wf, err)
			ok = false
		}
	}
	say("result: %s", map[bool]string{true: "success", false: "failure"}[ok])
	finish(ok)
}

// pickWorkflows returns the workflow files (relative to src) whose `on:` fires for ev, from the
// first workflow folder that exists.
func pickWorkflows(src string, ev event, say func(string, ...any)) []string {
	for _, dir := range []string{".github/workflows", ".gitea/workflows", ".forgejo/workflows"} {
		entries, err := os.ReadDir(filepath.Join(src, dir))
		if err != nil {
			continue
		}
		var picked []string
		for _, e := range entries {
			if e.IsDir() || !(strings.HasSuffix(e.Name(), ".yml") || strings.HasSuffix(e.Name(), ".yaml")) {
				continue
			}
			rel := filepath.Join(dir, e.Name())
			b, err := os.ReadFile(filepath.Join(src, rel))
			if err != nil {
				continue
			}
			fire, err := triggers(b, ev)
			if err != nil {
				say("%s: %v", rel, err)
				continue
			}
			if fire {
				picked = append(picked, rel)
			} else {
				say("%s: not for this event", rel)
			}
		}
		sort.Strings(picked)
		return picked
	}
	return nil
}

// isTag: cib reports a tag as a push whose "branch" is the tag's name. It is a tag if some peer's
// namespace in storage has a tag by that name and none has a branch by that name.
func isTag(storage, name string) bool {
	refs := func(kind string) bool {
		out, err := exec.Command("git", "--git-dir", storage, "for-each-ref", "--count=1",
			"--format=%(refname)", "refs/namespaces/*/refs/"+kind+"/"+name).Output()
		return err == nil && len(strings.TrimSpace(string(out))) > 0
	}
	return refs("tags") && !refs("heads")
}

func list(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' })
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
