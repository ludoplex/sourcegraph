// gitserver is the gitserver server.
package main // import "github.com/sourcegraph/sourcegraph/cmd/gitserver"

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/pkg/errors"

	"github.com/opentracing-contrib/go-stdlib/nethttp"
	"github.com/opentracing/opentracing-go"
	"gopkg.in/inconshreveable/log15.v2"

	"github.com/sourcegraph/sourcegraph/cmd/gitserver/server"
	"github.com/sourcegraph/sourcegraph/internal/debugserver"
	"github.com/sourcegraph/sourcegraph/internal/env"
	"github.com/sourcegraph/sourcegraph/internal/tracer"
)

var (
	reposDir          = env.Get("SRC_REPOS_DIR", "/data/repos", "Root dir containing repos.")
	runRepoCleanup, _ = strconv.ParseBool(env.Get("SRC_RUN_REPO_CLEANUP", "", "Periodically remove inactive repositories."))
	wantPctFree       = env.Get("SRC_REPOS_DESIRED_PERCENT_FREE", "10", "Target percentage of free space on disk.")
	janitorInterval   = env.Get("SRC_REPOS_JANITOR_INTERVAL", "1m", "Interval between cleanup runs")
)

func main() {
	env.Lock()
	env.HandleHelpFlag()
	tracer.Init()

	if reposDir == "" {
		log.Fatal("git-server: SRC_REPOS_DIR is required")
	}
	if err := os.MkdirAll(reposDir, os.ModePerm); err != nil {
		log.Fatalf("failed to create SRC_REPOS_DIR: %s", err)
	}

	wantPctFree2, err := parsePercent(wantPctFree)
	if err != nil {
		log.Fatalf("parsing $SRC_REPOS_DESIRED_PERCENT_FREE: %v", err)
	}
	gitserver := server.Server{
		ReposDir:                reposDir,
		DeleteStaleRepositories: runRepoCleanup,
		DesiredPercentFree:      wantPctFree2,
		StderrErrorLog:          maybeCreateStderrErrorLog(),
	}
	gitserver.RegisterMetrics()

	if tmpDir, err := gitserver.SetupAndClearTmp(); err != nil {
		log.Fatalf("failed to setup temporary directory: %s", err)
	} else {
		// Additionally set TMP_DIR so other temporary files we may accidently
		// create are on the faster RepoDir mount.
		os.Setenv("TMP_DIR", tmpDir)
	}

	// Create Handler now since it also initializes state
	handler := nethttp.Middleware(opentracing.GlobalTracer(), gitserver.Handler())

	go debugserver.Start()

	janitorInterval2, err := time.ParseDuration(janitorInterval)
	if err != nil {
		log.Fatalf("parsing $SRC_REPOS_JANITOR_INTERVAL: %v", err)
	}
	go func() {
		for {
			gitserver.Janitor()
			time.Sleep(janitorInterval2)
		}
	}()

	port := "3178"
	host := ""
	if env.InsecureDev {
		host = "127.0.0.1"
	}
	addr := net.JoinHostPort(host, port)
	srv := &http.Server{Addr: addr, Handler: handler}
	log15.Info("git-server: listening", "addr", srv.Addr)

	go func() {
		err := srv.ListenAndServe()
		if err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	// Listen for shutdown signals. When we receive one attempt to clean up,
	// but do an insta-shutdown if we receive more than one signal.
	c := make(chan os.Signal, 2)
	signal.Notify(c, syscall.SIGINT, syscall.SIGHUP)
	<-c
	go func() {
		<-c
		os.Exit(0)
	}()

	// Stop accepting requests. In the future we should use graceful shutdown.
	srv.Close()

	// The most important thing this does is kill all our clones. If we just
	// shutdown they will be orphaned and continue running.
	gitserver.Stop()
}

func parsePercent(s string) (int, error) {
	p, err := strconv.Atoi(s)
	if err != nil {
		return 0, errors.Wrap(err, "converting string to int")
	}
	if p < 0 {
		return 0, fmt.Errorf("negative value given for percentage: %d", p)
	}
	if p > 100 {
		return 0, fmt.Errorf("excessively high value given for percentage: %d", p)
	}
	return p, nil
}

// TODO(keegancsmith) remove! Temporary logging to understand errors in
// production. https://github.com/sourcegraph/sourcegraph/issues/6676
func maybeCreateStderrErrorLog() *log.Logger {
	// HACK runRepoCleanup is only set for sourcegraph.com
	if !runRepoCleanup {
		return nil
	}
	path := filepath.Join(reposDir, "git-stderr-error.log")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		log.Printf("WARN failed to open stderrErrorLog.log: %v", err)
		return nil
	}

	return log.New(f, "", log.LstdFlags)
}
