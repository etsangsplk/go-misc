// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Dashscrape is a tool to download build failure logs from the Go
// dashboard so they can be accessed and searched from the local file
// system.
//
// It organizes these logs into two directories created in the current
// working directory. The log/ directory contains all log files named
// the same way they are named by the dashboard (which happens to be
// the SHA-1 of their contents). The rev/ directory contains symlinks
// back to these logs named
//
//    rev/<ISO 8601 commit date>-<git revision>/<builder>
//
// Dashscrape will reuse existing log files and revision symlinks, so
// it only has to download logs that are new since the last time it
// was run.
//
// Dashscrape needs access to an up-to-date clone of the Go repository
// to resolve commit hashes to commit dates. This defaults to ~/go,
// but can be changed with the -C command line flag.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/golang/build/types"
)

var (
	goRepo   = flag.String("C", os.ExpandEnv("$HOME/go"), "Go repository `path`")
	nCommits = flag.Int("n", 300, "fetch logs for most recent `count` commits")
	par      = flag.Int("j", 5, "download `num` files concurrently")
)

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr,
			"Download recent build failure logs to the current directory.\n\n"+
				"For more information, see:\n"+
				"  http://godoc.org/github.com/aclements/go-misc/dashscrape/\n"+
				"Usage:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	// Create directory structure
	ensureDir("log")
	ensureDir("rev")

	// Set up fetchers
	fetchTokens = make(chan struct{}, *par)
	for i := 0; i < *par; i++ {
		fetchTokens <- struct{}{}
	}
	wg := sync.WaitGroup{}

	// Fetch dashboard pages
	haveCommits := 0
	for page := 0; haveCommits < *nCommits; page++ {
		// TODO: What if we go past the last page?
		url := fmt.Sprintf("http://build.golang.org/?mode=json&page=%d", page)
		<-fetchTokens
		fmt.Println("fetching", url)
		resp, err := http.Get(url)
		fetchTokens <- struct{}{}
		if err != nil {
			log.Fatal(err)
		}

		var status types.BuildStatus
		if err = json.NewDecoder(resp.Body).Decode(&status); err != nil {
			log.Fatal("unmarshalling result: ", err)
		}
		resp.Body.Close()

		for _, rev := range status.Revisions {
			haveCommits++
			if haveCommits > *nCommits {
				break
			}
			if rev.Repo != "go" {
				continue
			}
			for i, res := range rev.Results {
				if res == "" || res == "ok" {
					continue
				}

				wg.Add(1)
				go func(rev, builder, logURL string) {
					defer wg.Done()
					logPath, err := fetchLog(logURL)
					if err != nil {
						log.Fatal("fetching log: ", err)
					}
					if err := linkLog(rev, builder, logPath); err != nil {
						log.Fatal("linking log: ", err)
					}
				}(rev.Revision, status.Builders[i], res)
			}
		}
	}

	wg.Wait()

	// TODO: Record latest commit so we can fetch up to it and
	// stop. Or maybe it's so cheap to get the indexes that it
	// just doesn't matter if we download, say, 10 index pages.
}

// ensureDir creates directory name if it does not exist.
func ensureDir(name string) {
	err := os.Mkdir(name, 0777)
	if err != nil && !os.IsExist(err) {
		log.Fatal(err)
	}
}

type pendingFetch struct {
	err   error
	wchan chan struct{}
}

var fetchesLock sync.Mutex
var fetches = map[string]*pendingFetch{}

var fetchTokens chan struct{}

// fetchLog downloads the build log from logURL and returns the file
// path it was written to. If the destination file already exists,
// this returns immediately.
//
// This is safe to call concurrently. If multiple fetchLogs are called
// with the same log URL, they will all block until the log is saved
// to disk.
func fetchLog(logURL string) (string, error) {
	logPath := path.Join("log", path.Base(logURL))

	// Do we already have it?
	if _, err := os.Stat(logPath); err == nil {
		return logPath, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}

	// Check if another fetcher is working on it
	fetchesLock.Lock()
	if p, ok := fetches[logURL]; ok {
		fetchesLock.Unlock()
		<-p.wchan
		return logPath, p.err
	}

	p := &pendingFetch{wchan: make(chan struct{})}
	fetches[logURL] = p
	fetchesLock.Unlock()

	p.err = fetchLogNoSync(logURL, logPath)
	close(p.wchan)
	return logPath, p.err
}

func fetchLogNoSync(logURL, logPath string) error {
	<-fetchTokens
	fmt.Println("fetching", logURL)
	resp, err := http.Get(logURL)
	fetchTokens <- struct{}{}
	if err != nil {
		return err
	}

	if f, err := os.Create(logPath + ".tmp"); err != nil {
		return err
	} else {
		_, err := io.Copy(f, resp.Body)
		if err == nil {
			err = f.Sync()
		}
		resp.Body.Close()
		f.Close()
		if err != nil {
			os.Remove(logPath + ".tmp")
			return err
		}
	}
	if err := os.Rename(logPath+".tmp", logPath); err != nil {
		os.Remove(logPath + ".tmp")
		return err
	}

	return nil
}

// linkLog creates a symlink for finding logPath based on its git
// revision and builder.
func linkLog(revision, builder, logPath string) error {
	// Get revision date info
	date, err := revDate(revision)
	if err != nil {
		return err
	}

	// Create symlink
	out := path.Join("rev", date.Format("2006-01-02T15:04:05")+"-"+revision[:7])
	ensureDir(out)
	err = os.Symlink("../../"+logPath, path.Join(out, builder))
	if err != nil && !os.IsExist(err) {
		return err
	}

	return nil
}

// revDate returns the commit date of a git revision.
func revDate(revision string) (time.Time, error) {
	args := []string{"-C", *goRepo, "log", "-1", "--format=%cD", revision}
	out, err := exec.Command("git", args...).CombinedOutput()
	outs := string(out)
	if err != nil {
		if strings.Contains(outs, "bad object") {
			err = fmt.Errorf("unknown commit; try sync'ing your repository")
		}
		return time.Time{}, fmt.Errorf("git %v: %v", strings.Join(args, " "), err)
	}
	outs = strings.TrimRight(outs, "\n")
	return time.Parse("Mon, 2 Jan 2006 15:04:05 -0700", outs)
}
