package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"time"
)

// ownIP is this container's first non-loopback IPv4 address.
func ownIP() string {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && n.IP.To4() != nil {
			return n.IP.String()
		}
	}
	return "127.0.0.1"
}

// serveAPI answers the one GitHub REST call actions commonly make about their own repository,
// GET /repos/{owner}/{repo}, until the returned stop function is called.
func serveAPI(listen, name, description, defaultBranch, htmlURL string) (func(), error) {
	repo := map[string]any{
		"id": 0, "name": name, "full_name": "rad/" + name, "private": false,
		"owner":       map[string]any{"login": "rad", "type": "Organization"},
		"description": description, "default_branch": defaultBranch,
		"html_url": htmlURL, "license": nil, "topics": []string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(repo)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"Not Found: this is the Radicle CI node, not GitHub"}`))
	})
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}, nil
}
