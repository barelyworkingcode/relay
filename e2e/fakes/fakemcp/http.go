package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"relaye2e/fakes/calllog"
)

const maxBody = 10 << 20

func (s *server) serveHTTP(listen string, withOAuth bool, ttl time.Duration) error {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("listen %q: %w", listen, err)
	}
	base := "http://" + ln.Addr().String()

	mux := http.NewServeMux()
	var oa *oauthServer
	if withOAuth {
		oa = newOAuthServer(base, ttl, s.log)
		oa.mount(mux)
	}
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) { s.serveMCP(w, r, oa) })

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		_ = srv.Close()
	}()

	// The one stdout line is the readiness signal: it is printed only once the
	// listener accepts.
	fmt.Println(base + "/mcp")
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *server) serveMCP(w http.ResponseWriter, r *http.Request, oa *oauthServer) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	authHeader := r.Header.Get("Authorization")
	auth := calllog.AuthLabel(authHeader)

	var req rpcRequest
	var rep *reply
	if json.Unmarshal(body, &req) != nil {
		rep = s.parseFailure("http", body, auth)
	} else {
		// The request is logged even when it is about to be refused, so a test
		// can see that an unauthenticated call arrived.
		if oa != nil && !oa.validBearer(authHeader) {
			_ = s.log.Append(calllog.NewCall("http", req.Method, req.ID, req.Params, req.Params, auth))
			w.Header()["WWW-Authenticate"] = []string{fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource"`, oa.base)}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		rep = s.handle("http", req, auth)
	}
	if rep == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(rep.body)
}
