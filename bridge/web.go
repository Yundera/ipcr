package main

import (
	"embed"
	"encoding/json"
	"net/http"
	"os"
)

//go:embed web
var webFiles embed.FS

// webHandler serves the forge's public page. Everything on it is public anyway: links, the
// documentation, IPCR's list of published images (public on IPFS) and the list of mirrored
// repositories (public on Radicle). Nothing here changes anything.
func webHandler(st *state, publishedFile string) http.Handler {
	mux := http.NewServeMux()
	page, _ := webFiles.ReadFile("web/index.html")
	icon, _ := webFiles.ReadFile("web/icon.png")
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(page)
	})
	mux.HandleFunc("GET /icon.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Write(icon)
	})
	mux.HandleFunc("GET /images.json", func(w http.ResponseWriter, r *http.Request) {
		b, err := os.ReadFile(publishedFile)
		if err != nil {
			http.Error(w, "nothing published yet", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(b)
	})
	mux.HandleFunc("GET /repos.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(st.list())
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})
	return mux
}
