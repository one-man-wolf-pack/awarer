// Command siteserve serves a generated site directory for local inspection.
//
// It is a development tool and nothing else: it binds loopback by default, it
// never writes, and it is not part of any deployment. The generated site is
// plain static files, so a real host needs no runtime — this exists only so a
// person or a browser automation can look at the output the way a visitor would,
// including the directory-index routes that a file:// URL cannot resolve.
//
// Usage:
//
//	siteserve -dir site/dist [-addr 127.0.0.1:8080]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "siteserve: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	dir := flag.String("dir", "site/dist", "generated site directory to serve")
	addr := flag.String("addr", "127.0.0.1:8080", "address to listen on; loopback by default")
	flag.Parse()

	if flag.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flag.Arg(0))
	}
	abs, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("serving %s: %w (build it with `just site`)", *dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", abs)
	}

	root, err := os.OpenRoot(abs)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler(root),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(os.Stdout, "serving %s at http://%s/\n", abs, ln.Addr()); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt) //nolint:forbidigo // the process entrypoint of a dev tool; there is no caller context to inherit
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// handler serves the directory the way a static host does: a path ending in "/"
// is answered by its index.html, and an unknown path gets the site's own 404
// page with a 404 status, so the generated not-found page can be inspected in
// the shape a visitor would meet it.
//
// Every file in the tree is served, because every file in the tree answers a URL:
// the generator publishes no control file the deployment target would parse instead
// of returning, so there is nothing here to withhold.
func handler(root *os.Root) http.Handler {
	files := http.FileServerFS(root.FS())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(r.URL.Path, "/")
		if rel == "" || strings.HasSuffix(rel, "/") {
			rel += "index.html"
		}
		if _, err := root.Stat(rel); err != nil {
			notFound(w, root)
			return
		}
		files.ServeHTTP(w, r)
	})
}

// notFound answers with the generated 404 page.
func notFound(w http.ResponseWriter, root *os.Root) {
	body, err := root.ReadFile("404.html")
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write(body)
}
