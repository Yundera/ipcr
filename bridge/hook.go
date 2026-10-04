package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
)

// hookHandler receives Gitea's system webhook. It only learns *which* repository changed: the sync
// itself re-reads everything from Gitea, so a payload is never trusted for content, and a burst of
// deliveries for one repository collapses into one sync.
type hookHandler struct {
	secretFile string
	enqueue    func(id int64)
}

func (h *hookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	secret, _ := os.ReadFile(h.secretFile)
	if len(strings.TrimSpace(string(secret))) == 0 {
		http.Error(w, "not configured yet", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	if !validSignature([]byte(strings.TrimSpace(string(secret))), body, r.Header.Get("X-Gitea-Signature")) {
		log.Printf("hook: bad signature from %s (event %q)", r.RemoteAddr, r.Header.Get("X-Gitea-Event"))
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	var p struct {
		Repository *struct {
			ID int64 `json:"id"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &p); err != nil || p.Repository == nil || p.Repository.ID == 0 {
		// Signed, but about no repository (a user or organisation event): nothing to do.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h.enqueue(p.Repository.ID)
	// Answered at once: Gitea gives a delivery 5 seconds, a sync can take minutes.
	w.WriteHeader(http.StatusAccepted)
}

// validSignature checks Gitea's X-Gitea-Signature: hex HMAC-SHA256 of the raw body, no prefix.
func validSignature(secret, body []byte, sig string) bool {
	got, err := hex.DecodeString(strings.TrimSpace(sig))
	if err != nil || len(got) == 0 {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}
