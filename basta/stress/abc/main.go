// Based on c-p-b's merged-edge owner reproducer:
// https://gist.github.com/c-p-b/84857d8967147d400e045394231f55fc

// Command mergedowner-repro races three sessions against a buildkitd so that
// a job loads a vertex whose edge is already merged into another job's state,
// then cancels the owning jobs. On an affected daemon the surviving job fails
// with "failed to get state for index" or "inconsistent graph state".
//
// Each iteration, with fresh build-context content:
//
//  0. Warm: build the chain once, then prune the chain steps' results. Their
//     cache keys stay in the cache store, as a build-cache GC leaves them, so
//     in the race every chain step's edges find their keys and merge early.
//  1. A builds target TA on chain X(nameA): copy the context, then -depth RUN
//     steps.
//  2. B starts -gap later and builds TB on X(nameB). nameB differs from nameA,
//     so B's vertices have other digests but the same cache keys, and B's X
//     edges merge into A's states.
//  3. C starts -join after B and builds TC on X(nameB), loading B's states by
//     their exact digests after the merge.
//  4. A and B are canceled at -cancel. C must still finish.
//
// -cancel 0 is the control: nothing is canceled.
//
// Exit status: 0 when no C build failed, 1 when one did, 2 on a setup error.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/containerd/platforms"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	gw "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/tonistiigi/fsutil"
)

var (
	addr     = flag.String("addr", "tcp://127.0.0.1:1234", "buildkitd address")
	iters    = flag.Int("iters", 20, "iterations")
	depth    = flag.Int("depth", 3, "RUN steps in the shared chain")
	sleep    = flag.String("sleep", "1", "seconds each chain step runs")
	gap      = flag.Duration("gap", 300*time.Millisecond, "B starts this long after A")
	join     = flag.Duration("join", 300*time.Millisecond, "C starts this long after B")
	cancelAt = flag.Duration("cancel", 1800*time.Millisecond, "cancel A and B this long after A starts; 0 is the control")
	deadline = flag.Duration("deadline", time.Minute, "C build deadline; past it counts as a stall")
	image    = flag.String("image", "docker.io/library/alpine:3.20", "base image")
)

var platform llb.ConstraintsOpt

// lostState matches the solver's lookups of a vertex state that is gone.
func lostState(err error) bool {
	if err == nil {
		return false
	}
	for _, s := range []string{"failed to get state for index", "inconsistent graph state"} {
		if strings.Contains(err.Error(), s) {
			return true
		}
	}
	return false
}

// chain is X(local). The unique ID is the local name, so sessions using the
// same name load the same digests.
func chain(local string) llb.State {
	src := llb.Local(local, llb.SharedKeyHint("ctx"), llb.LocalUniqueID(local))
	st := llb.Image(*image, platform).File(llb.Copy(src, "/", "/src/", &llb.CopyInfo{CopyDirContentsOnly: true}))
	for i := 0; i < *depth; i++ {
		st = st.Run(llb.Shlex(fmt.Sprintf(`sh -c "sleep %s; echo s%d >> /src/h"`, *sleep, i))).Root()
	}
	return st
}

func target(local, tag string) llb.State {
	run := llb.Image(*image, platform).Run(llb.Shlex(fmt.Sprintf(`sh -c "cat /x/src/h > /out; echo %s >> /out"`, tag)))
	run.AddMount("/x", chain(local), llb.Readonly)
	return run.Root()
}

// build runs one session that solves target(local, tag) and reads the result,
// which forces execution.
func build(ctx context.Context, c *client.Client, dir, local, tag string) error {
	fs, err := fsutil.NewFS(dir)
	if err != nil {
		return err
	}
	opt := client.SolveOpt{LocalMounts: map[string]fsutil.FS{local: fs}}
	_, err = c.Build(ctx, opt, "", func(ctx context.Context, g gw.Client) (*gw.Result, error) {
		def, err := target(local, tag).Marshal(ctx, platform)
		if err != nil {
			return nil, err
		}
		res, err := g.Solve(ctx, gw.SolveRequest{Definition: def.ToPB()})
		if err != nil {
			return nil, err
		}
		ref, err := res.SingleRef()
		if err != nil {
			return nil, err
		}
		_, err = ref.ReadDir(ctx, gw.ReadDirRequest{Path: "/"})
		return gw.NewResult(), err
	}, nil)
	return err
}

// prune removes the results of the chain's RUN steps. Prune filters do not see
// a record's description, so records are picked here and pruned by ID.
func prune(ctx context.Context, c *client.Client) (int, error) {
	re := regexp.MustCompile(`echo s[0-9]+ >> /src/h`)
	du, err := c.DiskUsage(ctx)
	if err != nil {
		return 0, err
	}
	var ids []string
	for _, r := range du {
		if re.MatchString(r.Description) && !r.InUse {
			ids = append(ids, "id=="+r.ID)
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}
	ch := make(chan client.UsageInfo)
	done := make(chan int)
	go func() {
		n := 0
		for range ch {
			n++
		}
		done <- n
	}()
	err = c.Prune(ctx, ch, client.WithFilter(ids))
	close(ch)
	return <-done, err
}

// race runs one iteration and returns C's outcome: "." ok, "F" lost state,
// "S" stalled, "?" other error.
func race(ctx context.Context, c *client.Client, dir string, it int) string {
	now := time.Now().UnixNano()
	nameA := fmt.Sprintf("ctx-a-%d", now)
	nameB := fmt.Sprintf("ctx-b-%d", now)

	abctx, cancelAB := context.WithCancel(ctx)
	defer cancelAB()
	if *cancelAt > 0 {
		t := time.AfterFunc(*cancelAt, cancelAB)
		defer t.Stop()
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		build(abctx, c, dir, nameA, fmt.Sprintf("TA-%d", now))
	}()
	go func() {
		defer wg.Done()
		time.Sleep(*gap)
		build(abctx, c, dir, nameB, fmt.Sprintf("TB-%d", now))
	}()

	time.Sleep(*gap + *join)
	cctx, cancel := context.WithTimeout(ctx, *deadline)
	defer cancel()
	err := build(cctx, c, dir, nameB, fmt.Sprintf("TC-%d", now))
	wg.Wait()

	switch {
	case err == nil:
		return "."
	case cctx.Err() == context.DeadlineExceeded:
		fmt.Printf("iter %d: C stalled past %v\n", it, *deadline)
		return "S"
	case lostState(err):
		fmt.Printf("iter %d: C failed: %v\n", it, err)
		return "F"
	default:
		fmt.Printf("iter %d: C other error: %v\n", it, err)
		return "?"
	}
}

func main() {
	flag.Parse()
	ctx := context.Background()
	c, err := client.New(ctx, *addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	ws, err := c.ListWorkers(ctx)
	if err != nil || len(ws) == 0 || len(ws[0].Platforms) == 0 {
		fmt.Fprintln(os.Stderr, "listing workers:", err)
		os.Exit(2)
	}
	platform = llb.Platform(ws[0].Platforms[0])
	if info, err := c.Info(ctx); err == nil {
		fmt.Printf("daemon %s %s (%s), platform %s\n", info.BuildkitVersion.Package, info.BuildkitVersion.Version, info.BuildkitVersion.Revision, platforms.Format(ws[0].Platforms[0]))
	}

	dir, err := os.MkdirTemp("", "mergedowner-repro")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	defer os.RemoveAll(dir)

	counts := map[string]int{}
	for it := 0; it < *iters; it++ {
		content := fmt.Sprintf("iter %d %d\n", it, time.Now().UnixNano())
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(content), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if err := build(ctx, c, dir, fmt.Sprintf("ctx-warm-%d", time.Now().UnixNano()), "TW"); err != nil {
			fmt.Fprintf(os.Stderr, "iter %d: warm build: %v\n", it, err)
			os.Exit(2)
		}
		if _, err := prune(ctx, c); err != nil {
			fmt.Fprintf(os.Stderr, "iter %d: prune: %v\n", it, err)
			os.Exit(2)
		}
		r := race(ctx, c, dir, it)
		counts[r]++
		fmt.Printf("iter %d: C [%s]\n", it, r)
	}
	fmt.Printf("RESULT: %d of %d C builds lost state; %d stalled; %d other errors (cancel=%v)\n", counts["F"], *iters, counts["S"], counts["?"], *cancelAt)
	switch {
	case counts["F"] > 0 || counts["S"] > 0:
		os.Exit(1)
	case counts["?"] > 0:
		os.Exit(2)
	}
}
