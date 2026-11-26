// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"
)

// `gitea deptapp-static` is a static site's app process: it serves a folder
// on the app's socket, inside the sandbox, so a site of HTML, CSS and
// JavaScript is started, stopped, limited and health-checked like any other
// app. Like deptapp-exec it reads no config and opens no database.

func NewDeptAppStaticCommand() *cli.Command {
	return &cli.Command{
		Name:   "deptapp-static",
		Usage:  "(internal) Serve a static site as a department app",
		Hidden: true,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "socket", Required: true},
			&cli.StringFlag{Name: "root", Required: true},
		},
		Action: runDeptAppStatic,
	}
}

func runDeptAppStatic(ctx context.Context, cmd *cli.Command) error {
	root, err := os.OpenRoot(cmd.String("root")) // nothing outside the folder, symlinks included
	if err != nil {
		return err
	}
	defer root.Close()
	ln, err := net.Listen("unix", cmd.String("socket"))
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: staticSiteHandler(root.FS()), ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	_, _ = fmt.Fprintf(os.Stdout, "serving %s\n", cmd.String("root"))
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// staticSiteHandler serves files, a folder's index.html, and for a path that
// names no file and has no extension the site's own index.html, which is how
// a single-page app's routes load. Dotfiles are never served.
func staticSiteHandler(fsys fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		for seg := range strings.SplitSeq(name, "/") {
			if strings.HasPrefix(seg, ".") {
				http.NotFound(w, r)
				return
			}
		}
		candidates := []string{path.Join(name, "index.html")}
		if name != "" {
			candidates = []string{name, path.Join(name, "index.html")}
		}
		if path.Ext(name) == "" {
			candidates = append(candidates, "index.html")
		}
		for _, c := range candidates {
			if serveStaticFile(w, r, fsys, c) {
				return
			}
		}
		http.NotFound(w, r)
	})
}

func serveStaticFile(w http.ResponseWriter, r *http.Request, fsys fs.FS, name string) bool {
	f, err := fsys.Open(name)
	if err != nil {
		return false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	rs, ok := f.(interface {
		Read([]byte) (int, error)
		Seek(int64, int) (int64, error)
	})
	if !ok {
		return false
	}
	if path.Ext(name) == ".html" {
		w.Header().Set("Cache-Control", "no-cache") // a redeploy shows at once
	}
	http.ServeContent(w, r, name, fi.ModTime(), rs)
	return true
}
