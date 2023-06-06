// This is a fork under APC
// Original copyright:
// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// The stress utility is intended for catching sporadic failures.
// It runs a given process in parallel in a loop and collects any failures.
// Usage:
//
//	$ stress ./fmt.test -test.run=TestSometing -test.cpu=10
//
// You can also specify a number of parallel processes with -p flag;
// instruct the utility to not kill hanged processes for gdb attach;
// or specify the failure output you are looking for (if you want to
// ignore some other sporadic failures).
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/fatih/color"
)

var (
	flagMaxTime  = flag.Duration("max-time", 0, "exit successfully after this much time")
	flagMaxRuns  = flag.Int("max-runs", 0, "exit successfully after this many runs")
	flagP        = flag.Int("p", runtime.NumCPU(), "run `N` processes in parallel")
	flagLimit    = flag.Int("limit", 100, "maximum number of failures until exiting")
	flagTimeout  = flag.Duration("timeout", 10*time.Minute, "timeout each process after `duration`")
	flagKill     = flag.Bool("kill", true, "kill timed out processes if true, otherwise just print pid (to attach with gdb)")
	flagFailure  = flag.String("failure", "", "fail only if output matches `regexp`")
	flagIgnore   = flag.String("ignore", "", "ignore failure if output matches `regexp`")
	flagOutput   = flag.String("o", defaultPrefix(), "output failure logs to `path` plus a unique suffix")
	flagFailFast = flag.Bool("f", false, "exit on first failure")
)

func init() {
	flag.Usage = func() {
		_, _ = os.Stderr.WriteString(`The stress utility is intended for catching sporadic failures.
It runs a given process in parallel in a loop and collects any failures.
Usage:

	$ stress ./fmt.test -test.run=TestSometing -test.cpu=10

`)
		flag.PrintDefaults()
	}
}

func defaultPrefix() string {
	date := time.Now().Format("go-stress-20060102T150405-")
	return filepath.Join(os.TempDir(), date)
}

func latestName() string {
	return filepath.Join(os.TempDir(), "go-stress-latest")
}

type result struct {
	out      []byte
	duration time.Duration
}

func main() {
	flag.Parse()
	if *flagP <= 0 || *flagTimeout <= 0 || len(flag.Args()) == 0 {
		flag.Usage()
		os.Exit(1)
	}
	blue := color.New(color.FgBlue).SprintFunc()
	red := color.New(color.FgRed).SprintFunc()
	green := color.New(color.FgGreen).SprintFunc()
	var failureRe, ignoreRe *regexp.Regexp
	if *flagFailure != "" {
		var err error
		if failureRe, err = regexp.Compile(*flagFailure); err != nil {
			fmt.Println("bad failure regexp:", err)
			os.Exit(1)
		}
	}
	if *flagIgnore != "" {
		var err error
		if ignoreRe, err = regexp.Compile(*flagIgnore); err != nil {
			fmt.Println("bad ignore regexp:", err)
			os.Exit(1)
		}
	}
	results := make(chan result)
	max, min := time.Duration(0), time.Duration(1<<63-1)
	maxMu := sync.RWMutex{}
	for i := 0; i < *flagP; i++ {
		go func() {
			for {
				t0 := time.Now()
				cmd := exec.Command(flag.Args()[0], flag.Args()[1:]...)
				done := make(chan bool)
				if *flagTimeout > 0 {
					go func() {
						for {
							maxMu.RLock()
							m := max * 10
							maxMu.RUnlock()
							if m == 0 {
								m = *flagTimeout * 2
							}
							select {
							case <-done:
								return
							case <-time.After(*flagTimeout):
								break
							case <-time.After(m):
								fmt.Printf("process still running after %v\n", time.Since(t0))
							}
						}
						if !*flagKill {
							fmt.Printf("process %v timed out\n", cmd.Process.Pid)
							return
						}
						cmd.Process.Signal(syscall.SIGABRT)
						select {
						case <-done:
							return
						case <-time.After(10 * time.Second):
						}
						cmd.Process.Kill()
					}()
				}
				out, err := cmd.CombinedOutput()
				close(done)
				if err != nil && (failureRe == nil || failureRe.Match(out)) && (ignoreRe == nil || !ignoreRe.Match(out)) {
					out = append(out, fmt.Sprintf("\nERROR: %v", err)...)
				} else {
					out = []byte{}
				}
				results <- result{out, time.Since(t0)}
			}
		}()
	}
	runs, fails := 0, 0
	totalDuration := time.Duration(0)
	ticker := time.NewTicker(2 * time.Second).C
	displayProgress := func() {
		maxMu.RLock()
		defer maxMu.RUnlock()
		total := runs + fails
		if total == 0 {
			fmt.Printf("no runs so far\n")
		} else {
			fmt.Printf("%v runs so far, %v failures (%.2f%% pass rate). %v avg, %v max, %v min\n",
				runs, fails, 100.0*(float64(runs)/float64(total)), totalDuration/time.Duration(total), max, min)
		}
	}
	terminate := make(<-chan time.Time)
	if *flagMaxTime > 0 {
		terminate = time.After(*flagMaxTime)
	}
	for {
		select {
		case res := <-results:
			runs++
			totalDuration += res.duration
			maxMu.Lock()
			if res.duration > max {
				max = res.duration
			}
			maxMu.Unlock()
			if res.duration < min {
				min = res.duration
			}
			if runs == 1 {
				displayProgress()
			}
			if len(res.out) == 0 {
				if *flagMaxRuns > 0 && runs >= *flagMaxRuns {
					displayProgress()
					fmt.Printf(green("max runs hit, exiting\n"))
					if fails > 0 {
						os.Exit(1)
					}
					os.Exit(0)
				}
				continue
			}
			fails++
			displayProgress()
			dir, path := filepath.Split(*flagOutput)
			f, err := os.CreateTemp(dir, path)
			if err != nil {
				fmt.Printf("failed to create temp file: %v\n", err)
				os.Exit(1)
			}
			_ = os.WriteFile(f.Name(), res.out, 0o644)
			name := blue(f.Name())
			if len(res.out) > 2<<10 && !*flagFailFast {
				fmt.Printf("%s\n%s\n…\n", name, res.out[:2<<10])
			} else {
				fmt.Printf("%s\n%s\n", name, res.out)
			}
			// OK if it doesn't work
			// This is also racy - no problem, one will win.
			_ = os.Remove(latestName())
			_ = os.Symlink(f.Name(), latestName())
			if *flagFailFast {
				fmt.Printf(red("fail fast enabled, exiting\n"))
				os.Exit(1)
			}
			if fails >= *flagLimit {
				fmt.Printf(red("failure limit hit, exiting\n"))
				os.Exit(1)
			}
			if *flagMaxRuns > 0 && runs >= *flagMaxRuns {
				displayProgress()
				fmt.Printf(green("max runs hit, exiting\n"))
				if fails > 0 {
					os.Exit(1)
				}
				os.Exit(0)
			}
		case <-terminate:
			displayProgress()
			fmt.Printf(green("max time hit, exiting\n"))
			if fails > 0 {
				os.Exit(1)
			}
			os.Exit(0)
		case <-ticker:
			displayProgress()
		}
	}
}
