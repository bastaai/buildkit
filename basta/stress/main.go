// Based on c-p-b's merged-edge owner reproducer:
// https://gist.github.com/c-p-b/84857d8967147d400e045394231f55fc

// Command stress runs overlapping Earthly-like builds against a buildkitd:
// several workers each start a build that solves 3 of F "service" targets in
// parallel through the gateway, all sharing a common base step and per-family
// RUN chains. Every RUN gets one of three proxy values and the base image one
// of three ref spellings, so equal steps load with different digests but the
// same cache keys and their edges merge. A fraction of builds is canceled by
// the driver at a random time; a failing solve cancels the rest of its build,
// as Earth does. The generation changes every -gen so steps really execute.
//
// For each build NOT canceled by the driver the first error is classified:
// ok, lost (failed to get state / inconsistent graph state), canceled
// (gRPC Canceled or "context canceled"), stall (deadline), other.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	gw "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/tonistiigi/fsutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	addr        = flag.String("addr", "tcp://127.0.0.1:1234", "buildkitd address")
	workers     = flag.Int("workers", 8, "concurrent builds")
	duration    = flag.Duration("duration", 8*time.Minute, "how long to start builds")
	families    = flag.Int("families", 5, "service chains")
	perBuild    = flag.Int("per-build", 3, "services per build")
	depth       = flag.Int("depth", 3, "RUN steps per service chain")
	sleepS      = flag.Float64("sleep", 1.5, "seconds per RUN step")
	gen         = flag.Duration("gen", 15*time.Second, "generation length")
	cancelProb  = flag.Float64("cancel-prob", 0.35, "fraction of builds the driver cancels")
	cancelMax   = flag.Duration("cancel-max", 8*time.Second, "driver cancel happens in [300ms, this]")
	deadline    = flag.Duration("deadline", 3*time.Minute, "per-build deadline; past it counts as a stall")
	seed        = flag.Int64("seed", 1, "random seed")
	variants    = flag.Int("variants", 2, "digest variants per chain (max 3); same cache keys")
	cancelMin   = flag.Duration("cancel-min", 300*time.Millisecond, "earliest driver cancel")
	rounds      = flag.Bool("rounds", false, "round mode: warm+prune, then a staggered burst per round")
	roundsDur   = flag.Duration("rounds-duration", 8*time.Minute, "round mode duration")
	stagger     = flag.Duration("stagger", 300*time.Millisecond, "round mode: start gap between builds")
	gistPattern = flag.Bool("gist-pattern", false, "round mode: first three workers replay the gist's A/B/C")
	proxy       = flag.Bool("proxy", false, "also vary each RUN proxy env (digest changes, cache key does not)")
	verbose     = flag.Bool("v", false, "print every non-ok error")
)

var imageRefs = []string{"alpine:3.20", "docker.io/library/alpine:3.20", "docker.io/alpine:3.20"}
var proxies = []string{"http://p0.invalid:1", "http://p1.invalid:1", "http://p2.invalid:1"}

var platform llb.ConstraintsOpt
var ctxDir string

func run(st llb.State, v int, cmd string) llb.State {
	opts := []llb.RunOption{llb.Shlex(fmt.Sprintf(`sh -c %q`, cmd))}
	if *proxy {
		opts = append(opts, llb.WithProxy(llb.ProxyEnv{HTTPProxy: proxies[v]}))
	}
	return st.Run(opts...).Root()
}

var tagSeq atomic.Int64

// service is family f of generation g, spelled with random variants.
func service(r *rand.Rand, g int64, f, v int) llb.State {
	if v < 0 {
		v = r.Intn(*variants)
	}
	src := llb.Local(fmt.Sprintf("ctx-v%d", v), llb.SharedKeyHint("ctx"), llb.LocalUniqueID(fmt.Sprintf("ctx-v%d", v)))
	st := llb.Image(imageRefs[v], platform).File(llb.Copy(src, "/", "/src/", &llb.CopyInfo{CopyDirContentsOnly: true}))
	st = run(st, v, fmt.Sprintf("sleep %g; echo base-%d > /base", *sleepS, g))
	for i := 0; i < *depth; i++ {
		st = run(st, v, fmt.Sprintf("sleep %g; echo g%d-f%d-s%d >> /h", *sleepS, g, f, i))
	}
	top := llb.Image(imageRefs[v], platform).Run(llb.Shlex(fmt.Sprintf(`sh -c "cat /x/base /x/h > /compose.yml; echo f%d-t%d >> /compose.yml"`, f, tagSeq.Add(1))))
	top.AddMount("/x", st, llb.Readonly)
	return top.Root()
}

type outcome struct {
	driverCanceled bool
	err            error
	stalled        bool
}

func isCanceled(err error) bool {
	if err == nil {
		return false
	}
	if s, ok := status.FromError(err); ok && s.Code() == codes.Canceled {
		return true
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	return strings.Contains(err.Error(), "context canceled") || strings.Contains(err.Error(), "Canceled")
}

func isLost(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "failed to get state") || strings.Contains(err.Error(), "inconsistent graph state") || strings.Contains(err.Error(), "failed to get edge"))
}

func randCancel(r *rand.Rand) time.Duration {
	if r.Float64() < *cancelProb {
		return *cancelMin + time.Duration(r.Int63n(int64(*cancelMax-*cancelMin)))
	}
	return 0
}

func build(parent context.Context, c *client.Client, r *rand.Rand, g int64, fams []int, cancelAt time.Duration, v int) outcome {
	ctx, cancel := context.WithTimeout(parent, *deadline)
	defer cancel()
	var driverCanceled atomic.Bool
	if cancelAt > 0 {
		t := time.AfterFunc(cancelAt, func() { driverCanceled.Store(true); cancel() })
		defer t.Stop()
	}
	defs := make([]llb.State, len(fams))
	for i, f := range fams {
		defs[i] = service(r, g, f, v)
	}

	var first error
	var firstMu sync.Mutex
	mounts := map[string]fsutil.FS{}
	for i := 0; i < 3; i++ {
		fs, err := fsutil.NewFS(ctxDir)
		if err != nil {
			return outcome{err: err}
		}
		mounts[fmt.Sprintf("ctx-v%d", i)] = fs
	}
	_, err := c.Build(ctx, client.SolveOpt{LocalMounts: mounts}, "", func(ctx context.Context, gc gw.Client) (*gw.Result, error) {
		bctx, bcancel := context.WithCancel(ctx)
		defer bcancel()
		var wg sync.WaitGroup
		for i := range defs {
			wg.Add(1)
			go func(st llb.State) {
				defer wg.Done()
				err := func() error {
					def, err := st.Marshal(bctx, platform)
					if err != nil {
						return err
					}
					res, err := gc.Solve(bctx, gw.SolveRequest{Definition: def.ToPB()})
					if err != nil {
						return fmt.Errorf("solve: %w", err)
					}
					ref, err := res.SingleRef()
					if err != nil {
						return err
					}
					_, err = ref.ReadFile(bctx, gw.ReadRequest{Filename: "/compose.yml"})
					if err != nil {
						return fmt.Errorf("read compose config file: %w", err)
					}
					return nil
				}()
				if err != nil {
					firstMu.Lock()
					if first == nil && !driverCanceled.Load() {
						first = err
					}
					firstMu.Unlock()
					bcancel() // Earth cancels every target when one fails
				}
			}(defs[i])
		}
		wg.Wait()
		firstMu.Lock()
		defer firstMu.Unlock()
		return gw.NewResult(), first
	}, nil)
	o := outcome{driverCanceled: driverCanceled.Load()}
	if first != nil {
		o.err = first
	} else {
		o.err = err
	}
	if o.err != nil && !o.driverCanceled && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		o.stalled = true
	}
	return o
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
	if err != nil || len(ws) == 0 {
		fmt.Fprintln(os.Stderr, "listing workers:", err)
		os.Exit(2)
	}
	platform = llb.Platform(ws[0].Platforms[0])
	if info, err := c.Info(ctx); err == nil {
		fmt.Printf("daemon %s (%s)\n", info.BuildkitVersion.Version, info.BuildkitVersion.Revision)
	}

	ctxDir, err = os.MkdirTemp("", "stress-ctx")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(ctxDir)
	os.WriteFile(filepath.Join(ctxDir, "a.txt"), []byte("x"), 0o644)
	start := time.Now()
	if *rounds {
		*duration = 0
	}
	var wg sync.WaitGroup
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(*seed*1000 + int64(w)))
			for n := 0; time.Since(start) < *duration; n++ {
				g := time.Now().UnixNano() / int64(*gen)
				o := build(ctx, c, r, g, r.Perm(*families)[:*perBuild], randCancel(r), -1)
				record(w, n, o)
			}
		}(w)
	}
	wg.Wait()
	if *rounds {
		runRounds(ctx, c)
	}
	total := counts["ok"] + counts["lost"] + counts["canceled"] + counts["other"] + counts["stall"]
	fmt.Printf("RESULT: not-driver-canceled=%d ok=%d lost=%d canceled=%d stall=%d other=%d driver-canceled=%d\n",
		total, counts["ok"], counts["lost"], counts["canceled"], counts["stall"], counts["other"], counts["driver-canceled"])
}

var (
	mu     sync.Mutex
	counts = map[string]int{}
)

func record(w, n int, o outcome) {
	k := "ok"
	switch {
	case o.driverCanceled:
		k = "driver-canceled"
	case o.err == nil:
	case o.stalled:
		k = "stall"
	case isLost(o.err):
		k = "lost"
	case isCanceled(o.err):
		k = "canceled"
	default:
		k = "other"
	}
	mu.Lock()
	counts[k]++
	mu.Unlock()
	if k != "ok" && k != "driver-canceled" {
		fmt.Printf("%s w%d #%d %s: %v\n", time.Now().Format("15:04:05"), w, n, k, o.err)
	} else if *verbose && o.err != nil && !o.driverCanceled {
		fmt.Printf("w%d #%d %s: %v\n", w, n, k, o.err)
	}
}

func runRounds(ctx context.Context, c *client.Client) {
	r := rand.New(rand.NewSource(*seed))
	all := make([]int, *families)
	for i := range all {
		all[i] = i
	}
	start := time.Now()
	for round := 0; time.Since(start) < *roundsDur; round++ {
		g := time.Now().UnixNano()
		if err := os.WriteFile(filepath.Join(ctxDir, "a.txt"), []byte(fmt.Sprint(g)), 0o644); err != nil {
			panic(err)
		}
		if o := build(ctx, c, r, g, all, 0, -1); o.err != nil {
			fmt.Printf("round %d warm: %v\n", round, o.err)
			continue
		}
		if err := prune(ctx, c); err != nil {
			fmt.Printf("round %d prune: %v\n", round, err)
		}
		var wg sync.WaitGroup
		for w := 0; w < *workers; w++ {
			fams := r.Perm(*families)[:*perBuild]
			at := randCancel(r)
			v := -1
			if *gistPattern && w < 3 {
				// A: v0 canceled, B: v1 canceled, C: v1 must finish
				fams, v, at = all[:*perBuild], []int{0, 1, 1}[w], []time.Duration{*cancelMax, *cancelMax - *stagger, 0}[w]
			}
			seedW := r.Int63()
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				time.Sleep(time.Duration(w) * *stagger)
				t0 := time.Now()
				o := build(ctx, c, rand.New(rand.NewSource(seedW)), g, fams, at, v)
				if *verbose {
					fmt.Printf("round %d w%d v=%d cancel=%v took %v err=%v\n", round, w, v, at, time.Since(t0).Round(time.Millisecond), o.err)
				}
				record(w, round, o)
			}(w)
		}
		wg.Wait()
	}
}

var pruneRe = regexp.MustCompile(`echo (base-|g[0-9]+-f)|cat /base`)

func prune(ctx context.Context, c *client.Client) error {
	du, err := c.DiskUsage(ctx)
	if err != nil {
		return err
	}
	var ids []string
	for _, r := range du {
		if pruneRe.MatchString(r.Description) && !r.InUse {
			ids = append(ids, "id=="+r.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	ch := make(chan client.UsageInfo)
	go func() {
		for range ch {
		}
	}()
	err = c.Prune(ctx, ch, client.WithFilter(ids))
	close(ch)
	if *verbose {
		fmt.Printf("pruned %d of %d records\n", len(ids), len(du))
	}
	return err
}
