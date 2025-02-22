// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
// Usage:
//
//	goproxy [-listen [host]:port] [-cacheDir /tmp]
//
// goproxy serves the Go module proxy HTTP protocol at the given address (default 0.0.0.0:8081).
// It invokes the local go command to answer requests and therefore reuses
// the current GOPATH's module download cache and configuration (GOPROXY, GOSUMDB, and so on).
//
// While the proxy is running, setting GOPROXY=http://host:port will instruct the go command to use it.
// Note that the module proxy cannot share a GOPATH with its own clients or else fetches will deadlock.
// (The client will lock the entry as “being downloaded” before sending the request to the proxy,
// which will then wait for the apparently-in-progress download to finish.)
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/goproxyio/goproxy/v2/proxy"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/mod/module"
)

var downloadRoot string

const listExpire = proxy.ListExpire

var listen, promListen string
var cacheDir string
var proxyHost string
var excludeHost string
var offLine bool

func init() {
	flag.StringVar(&excludeHost, "exclude", "", "exclude host pattern, you can exclude internal Git services")
	flag.StringVar(&proxyHost, "proxy", "", "next hop proxy for Go Modules, recommend use https://gopropxy.io")
	flag.StringVar(&cacheDir, "cacheDir", "", "Go Modules cache dir, default is $GOPATH/pkg/mod/cache/download")
	flag.StringVar(&listen, "listen", "0.0.0.0:8081", "service listen address")
	flag.BoolVar(&offLine, "offline", false, "Offline mode, use cache only")
	flag.Parse()

	if os.Getenv("GIT_TERMINAL_PROMPT") == "" {
		os.Setenv("GIT_TERMINAL_PROMPT", "0")
	}

	if os.Getenv("GIT_SSH") == "" && os.Getenv("GIT_SSH_COMMAND") == "" {
		os.Setenv("GIT_SSH_COMMAND", "ssh -o ControlMaster=no")
	}

	if excludeHost != "" {
		os.Setenv("GOPRIVATE", excludeHost)
	}

	// Enable Go module
	os.Setenv("GO111MODULE", "on")
	os.Setenv("GOPROXY", "direct")
	os.Setenv("GOSUMDB", "off")

	downloadRoot = getDownloadRoot()
}

func main() {
	log.SetPrefix("goproxy.io: ")
	log.SetFlags(0)

	var handle http.Handler
	if proxyHost != "" {
		log.Printf("ProxyHost %s\n", proxyHost)
		if excludeHost != "" {
			log.Printf("ExcludeHost %s\n", excludeHost)
		}
		handle = &logger{proxy.NewRouter(proxy.NewServer(new(onlineOps)), &proxy.RouterOptions{
			Pattern:      excludeHost,
			Proxy:        proxyHost,
			DownloadRoot: downloadRoot,
		})}
	} else {
		if offLine {
			handle = &logger{proxy.NewServer(new(offlineOps))}
		} else {
			handle = &logger{proxy.NewServer(new(onlineOps))}
		}
	}

	server := &http.Server{Addr: listen, Handler: handle}
	go func() {
		if err := server.ListenAndServe(); err != nil {
			if err != http.ErrServerClosed {
				log.Fatal(err)
			}
		}
	}()

	s := make(chan os.Signal, 1)
	signal.Notify(s, os.Interrupt, syscall.SIGTERM)
	<-s
	log.Println("Making a graceful shutdown...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := server.Shutdown(ctx)
	if err != nil {
		log.Fatalf("Error while shutting down the server: %v", err)
	}
	log.Println("Successful server shutdown.")
}

func getDownloadRoot() string {
	var env struct {
		GOPATH string
	}
	if cacheDir != "" {
		os.Setenv("GOMODCACHE", filepath.Join(cacheDir, "pkg", "mod"))
		return filepath.Join(cacheDir, "pkg", "mod", "cache", "download")
	}
	if err := goJSON(&env, "go", "env", "-json", "GOPATH"); err != nil {
		log.Fatal(err)
	}
	list := filepath.SplitList(env.GOPATH)
	if len(list) == 0 || list[0] == "" {
		log.Fatalf("missing $GOPATH")
	}
	os.Setenv("GOMODCACHE", filepath.Join(list[0], "pkg", "mod"))
	return filepath.Join(list[0], "pkg", "mod", "cache", "download")
}

// goJSON runs the go command and parses its JSON output into dst.
func goJSON(dst interface{}, command ...string) error {
	cmd := exec.Command(command[0], command[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s:\n%s%s", strings.Join(command, " "), stderr.String(), stdout.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), dst); err != nil {
		return fmt.Errorf("%s: reading json: %v", strings.Join(command, " "), err)
	}
	return nil
}

// A logger is an http.Handler that logs traffic to standard error.
type logger struct {
	h http.Handler
}
type responseLogger struct {
	code int
	http.ResponseWriter
}

// WriteHeader writes header code into responser writer.
func (r *responseLogger) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

// ServeHTTP implements http handler.
func (l *logger) ServeHTTP(w http.ResponseWriter, r *http.Request) {

	// Prometheus metrics
	if r.URL.Path == "/metrics" {
		promhttp.Handler().ServeHTTP(w, r)
		return
	}

	start := time.Now()
	rl := &responseLogger{code: 200, ResponseWriter: w}
	l.h.ServeHTTP(rl, r)
	log.Printf("%.3fs %d %s\n", time.Since(start).Seconds(), rl.code, r.URL)
}

// An onlineOps is a proxy.ServerOps implementation.
type onlineOps struct{}

// NewContext creates a context.
func (*onlineOps) NewContext(r *http.Request) (context.Context, error) {
	return context.Background(), nil
}

// List lists proxy files.
func (*onlineOps) List(ctx context.Context, mpath string) (proxy.File, error) {
	escMod, err := module.EscapePath(mpath)
	if err != nil {
		return nil, err
	}
	file := filepath.Join(downloadRoot, escMod, "@v", "list")
	if info, err := os.Stat(file); err == nil && time.Since(info.ModTime()) < listExpire {
		return os.Open(file)
	}
	var list struct {
		Path     string
		Versions []string
	}
	if err := goJSON(&list, "go", "list", "-m", "-json", "-versions", mpath+"@latest"); err != nil {
		return nil, err
	}
	if list.Path != mpath {
		return nil, fmt.Errorf("go list -m: asked for %s but got %s", mpath, list.Path)
	}
	data := []byte(strings.Join(list.Versions, "\n") + "\n")
	if len(data) == 1 {
		data = nil
	}
	err = os.MkdirAll(path.Dir(file), os.ModePerm)
	if err != nil {
		log.Printf("make cache dir failed, err: %v.", err)
		return nil, err
	}
	if err := ioutil.WriteFile(file, data, 0666); err != nil {
		return nil, err
	}

	return os.Open(file)
}

// Latest fetches latest file.
func (*onlineOps) Latest(ctx context.Context, path string) (proxy.File, error) {
	d, err := download(module.Version{Path: path, Version: "latest"})
	if err != nil {
		return nil, err
	}
	return os.Open(d.Info)
}

// Info fetches info file.
func (*onlineOps) Info(ctx context.Context, m module.Version) (proxy.File, error) {
	d, err := download(m)
	if err != nil {
		return nil, err
	}
	return os.Open(d.Info)
}

// GoMod fetches go mod file.
func (*onlineOps) GoMod(ctx context.Context, m module.Version) (proxy.File, error) {
	d, err := download(m)
	if err != nil {
		return nil, err
	}
	return os.Open(d.GoMod)
}

// Zip fetches zip file.
func (*onlineOps) Zip(ctx context.Context, m module.Version) (proxy.File, error) {
	d, err := download(m)
	if err != nil {
		return nil, err
	}
	return os.Open(d.Zip)
}

type downloadInfo struct {
	Path     string
	Version  string
	Info     string
	GoMod    string
	Zip      string
	Dir      string
	Sum      string
	GoModSum string
}

func download(m module.Version) (*downloadInfo, error) {
	d := new(downloadInfo)
	return d, goJSON(d, "go", "mod", "download", "-json", m.String())
}

// An offlineOps is a proxy.ServerOps implementation.
type offlineOps struct{
	onlineOps
}

// List lists proxy files.
func (*offlineOps) List(ctx context.Context, mpath string) (proxy.File, error) {
	escMod, err := module.EscapePath(mpath)
	if err != nil {
		return nil, err
	}
	file := filepath.Join(downloadRoot, escMod, "@v", "list")
	if _, err := os.Stat(file); err == nil {
		return os.Open(file)
	} else {
		return nil, err
	}
}

// Latest fetches latest file.
func (*offlineOps) Latest(ctx context.Context, path string) (proxy.File, error) {
	return getOfflineFile(module.Version{Path: path, Version: "latest"}, ".info")
}

// Info fetches info file.
func (*offlineOps) Info(ctx context.Context, m module.Version) (proxy.File, error) {
	return getOfflineFile(m, ".info")
}

// GoMod fetches go mod file.
func (*offlineOps) GoMod(ctx context.Context, m module.Version) (proxy.File, error) {
	return getOfflineFile(m, ".mod")
}

// Zip fetches zip file.
func (*offlineOps) Zip(ctx context.Context, m module.Version) (proxy.File, error) {
	return getOfflineFile(m, ".zip")
}

func getOfflineFile(m module.Version, suffix string) (proxy.File, error) {
	escMod, err := module.EscapePath(m.Path)
	if err != nil {
		return nil, err
	}
	file := filepath.Join(downloadRoot, escMod, "@v", m.Version+suffix)
	if _, err := os.Stat(file); err == nil {
		return os.Open(file)
	} else {
		return nil, err
	}
}
