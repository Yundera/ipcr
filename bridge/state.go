package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Repository states, as shown on the page.
const (
	stSynced  = "synced"  // Radicle has what Gitea has
	stError   = "error"   // the last sync failed; retried on the next event or reconcile
	stWaiting = "waiting" // empty repository: nothing to publish yet
	stFrozen  = "frozen"  // no longer public in Gitea (or deleted): no further sync, nothing retracted
)

// repoState is what the bridge remembers per Gitea repository, keyed by Gitea's numeric ID so a
// rename or a transfer keeps its Radicle ID.
type repoState struct {
	ID            int64     `json:"id"`
	FullName      string    `json:"full_name"`
	RID           string    `json:"rid,omitempty"`
	DefaultBranch string    `json:"default_branch,omitempty"`
	State         string    `json:"state"`
	Error         string    `json:"error,omitempty"`
	LastSync      time.Time `json:"last_sync,omitzero"`
	// A digest of the refs last pushed: a reconcile that finds the same refs pushes nothing.
	Refs string `json:"refs,omitempty"`
	// The canonical-tags rule is in the repository's identity (see rad.go).
	Crefs bool `json:"crefs,omitempty"`
}

type state struct {
	mu    sync.Mutex
	path  string
	repos map[int64]*repoState
}

func loadState(path string) (*state, error) {
	s := &state{path: path, repos: map[int64]*repoState{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var m map[string]*repoState
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	for _, r := range m {
		s.repos[r.ID] = r
	}
	return s, nil
}

// get returns a copy of the repository's state (a zero one, with the ID set, when unknown).
func (s *state) get(id int64) repoState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.repos[id]; ok {
		return *r
	}
	return repoState{ID: id}
}

func (s *state) known(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.repos[id]
	return ok
}

// put stores r and writes the file (renamed into place, so a reader never sees half of it).
func (s *state) put(r repoState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repos[r.ID] = &r
	m := make(map[string]*repoState, len(s.repos))
	for id, v := range s.repos {
		m[strconv.FormatInt(id, 10)] = v
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(s.path), "."+filepath.Base(s.path)+".new")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *state) ids() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]int64, 0, len(s.repos))
	for id := range s.repos {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// list is the page's view: every repository, by name, without internal bookkeeping.
func (s *state) list() []repoState {
	s.mu.Lock()
	out := make([]repoState, 0, len(s.repos))
	for _, r := range s.repos {
		v := *r
		v.Refs = ""
		out = append(out, v)
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].FullName < out[j].FullName })
	return out
}
