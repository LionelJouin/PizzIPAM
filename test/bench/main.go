/*
Copyright (c) 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Command bench measures IPSlice admission latency against a real cluster.
//
// It answers two questions:
//
//  1. Does each successive request cost MORE at admission time? The allocator's
//     CEL scans existing allocations and enumerates offsets on every write, so
//     latency may grow as the slice fills. This fills a /26 one request per write
//     (the allocator serves one new request per pass) and prints each write's
//     latency, plus early-vs-late means and a rough per-request slope.
//
//  2. How does that compare to creating an empty IPSlice (no request)? That is
//     the cheapest admission path — the allocator has nothing to place — so it is
//     the baseline the fill writes are measured against.
//
// Usage (needs a cluster with ./deployment applied, like the e2e suite):
//
//	go run ./test/bench                 # baseline x20, fill a full /26 (62)
//	go run ./test/bench -fill 30 -baseline-samples 50
//	go run ./test/bench -keep           # don't delete the objects afterwards
package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	v1alpha1 "github.com/lioneljouin/pizzipam/apis/v1alpha1"
	versioned "github.com/lioneljouin/pizzipam/pkg/client/clientset/versioned"
	"github.com/lioneljouin/pizzipam/pkg/naming"
)

// metricFamilies are the apiserver metrics we diff around the fill to see the
// SERVER-side cost (CEL execution and CPU), independent of client wall-clock,
// network and rate limiting. Histograms are read via their _sum/_count series.
var metricFamilies = []string{
	"apiserver_admission_controller_admission_duration_seconds", // per-plugin admit/validate (CEL policies show as MutatingAdmissionPolicy/ValidatingAdmissionPolicy)
	"apiserver_admission_step_admission_duration_seconds",       // whole mutating/validating step
	"apiserver_admission_webhook_admission_duration_seconds",    // webhooks, for a side-by-side
	"apiserver_request_duration_seconds",                        // TOTAL server time per request; minus the admission step this exposes CRD x-kubernetes-validations + etcd
	"process_cpu_seconds_total",                                 // apiserver CPU consumed
}

// noiseFloor hides the dozens of built-in admission plugins that run in ~0-1µs,
// so the output shows only the series that actually cost something.
const noiseFloor = 30 * time.Microsecond

const (
	// 192.168.0.0; each /26 slice is 64 addresses aligned to a multiple of 64.
	baseNetwork = int64(3232235520)
	sliceStep   = int64(64) // distance between successive /26 network addresses
)

func main() {
	var (
		fill            = flag.Int("fill", 62, "number of requests to add one at a time (a /26 has 62 usable)")
		baselineSamples = flag.Int("baseline-samples", 20, "how many empty IPSlices to create for the baseline")
		keep            = flag.Bool("keep", false, "do not delete the created IPSlices at the end")
		qps             = flag.Float64("qps", 1000, "client-go REST QPS; the DEFAULT of 5 throttles to ~200ms/request and hides the real admission latency. Keep high to measure the server.")
		metrics         = flag.Bool("metrics", true, "scrape apiserver /metrics around the fill to report SERVER-side CEL cost and CPU (needs access to /metrics)")
	)
	flag.Parse()

	if err := run(*fill, *baselineSamples, *keep, float32(*qps), *metrics); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(fill, baselineSamples int, keep bool, qps float32, metrics bool) error {
	ctx := context.Background()

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules, &clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		return fmt.Errorf("load kubeconfig (need a running cluster): %w", err)
	}
	// The default client rate limiter (QPS 5, Burst 10) throttles to ~200ms per
	// request once the burst is spent, which swamps the real admission latency.
	// Raise both so we measure the server, not the client's token bucket.
	cfg.QPS = qps
	cfg.Burst = int(qps) + 1
	client, err := versioned.NewForConfig(cfg)
	if err != nil {
		return err
	}
	ips := client.MultinetworkV1alpha1().IPSlices()

	// A plain clientset is only used to GET /metrics for the server-side view.
	var kube *kubernetes.Clientset
	if metrics {
		if kube, err = kubernetes.NewForConfig(cfg); err != nil {
			return err
		}
	}

	// Track every object we create so we can clean up even on partial failure.
	var toDelete []string
	defer func() {
		if keep {
			fmt.Printf("\n-keep set: leaving %d IPSlices in the cluster\n", len(toDelete))
			return
		}
		for _, name := range toDelete {
			_ = ips.Delete(ctx, name, metav1.DeleteOptions{})
		}
	}()

	// ---- Baseline: create an empty IPSlice (no request) ---------------------
	// Each sample uses its own /26 so there are no name collisions and no waiting
	// on async deletes between iterations.
	fmt.Printf("baseline: creating %d empty IPSlices (no request)\n", baselineSamples)
	baseline := make([]time.Duration, 0, baselineSamples)
	for i := 0; i < baselineSamples; i++ {
		net := baseNetwork + int64(i)*sliceStep
		obj := newSlice(net)

		start := time.Now()
		created, err := ips.Create(ctx, obj, metav1.CreateOptions{})
		d := time.Since(start)
		if err != nil {
			return fmt.Errorf("baseline create %d: %w", i, err)
		}
		toDelete = append(toDelete, created.Name)
		baseline = append(baseline, d)
	}
	printStats("baseline empty-create", baseline)

	// ---- Fill: add one request per write and time each write ----------------
	// Use a /26 far from the baseline block so its name cannot collide.
	fillNet := baseNetwork + int64(baselineSamples+8)*sliceStep
	fmt.Printf("\nfill: creating a /26 and adding %d requests one at a time\n", fill)

	// Snapshot apiserver metrics right before the fill so we can attribute the
	// server-side cost (CEL execution + CPU) to exactly this workload.
	var before map[string]float64
	if metrics {
		if before, err = scrapeMetrics(ctx, kube); err != nil {
			fmt.Printf("  (metrics unavailable, skipping server-side view: %v)\n", err)
			metrics = false
		}
	}

	obj := newSlice(fillNet)
	obj.Spec.Request = []v1alpha1.Request{{Name: "req-0"}}
	start := time.Now()
	created, err := ips.Create(ctx, obj, metav1.CreateOptions{})
	firstCreate := time.Since(start)
	if err != nil {
		return fmt.Errorf("fill create: %w", err)
	}
	toDelete = append(toDelete, created.Name)
	fmt.Printf("  create with req-0 (1 allocation): %v\n", round(firstCreate))

	steps := make([]time.Duration, 0, fill)
	for i := 1; i < fill; i++ {
		created.Spec.Request = append(created.Spec.Request,
			v1alpha1.Request{Name: fmt.Sprintf("req-%d", i)})

		start := time.Now()
		created, err = ips.Update(ctx, created, metav1.UpdateOptions{})
		d := time.Since(start)
		if err != nil {
			return fmt.Errorf("fill update %d (%d allocations so far): %w", i, i, err)
		}
		steps = append(steps, d)
		fmt.Printf("  update -> %2d allocations: %v\n", i+1, round(d))
	}

	fmt.Println()
	printStats("fill per-write", steps)

	// ---- Does it grow? Compare the first writes to the last writes ----------
	if len(steps) >= 4 {
		n := len(steps) / 4 // compare first quarter vs last quarter
		early := mean(steps[:n])
		late := mean(steps[len(steps)-n:])
		fmt.Printf("\ngrowth check (first %d writes vs last %d writes):\n", n, n)
		fmt.Printf("  early mean: %v\n", round(early))
		fmt.Printf("  late  mean: %v\n", round(late))
		fmt.Printf("  delta:      %v  (%+.1f%%)\n", round(late-early), pct(early, late))
		fmt.Printf("  slope:      ~%v per additional request\n",
			round((late-early)/time.Duration(len(steps)-n)))
	}

	// ---- Baseline vs fill ---------------------------------------------------
	fmt.Printf("\nbaseline empty-create mean: %v\n", round(mean(baseline)))
	fmt.Printf("fill per-write mean:        %v  (%+.1f%% vs baseline)\n",
		round(mean(steps)), pct(mean(baseline), mean(steps)))

	// ---- Server-side view: what the apiserver actually paid -----------------
	// This is the number that matters for "impact on the component executing the
	// CEL": it is measured inside the apiserver, so it excludes network, etcd and
	// client rate limiting. Compare the per-admission mean here to the wall-clock
	// above -- the gap is everything that is NOT CEL.
	if metrics {
		after, err := scrapeMetrics(ctx, kube)
		if err != nil {
			fmt.Printf("\n(could not re-scrape metrics: %v)\n", err)
		} else {
			printMetricDiff(before, after, fill)
		}
	}

	return nil
}

// scrapeMetrics GETs the apiserver /metrics and returns the value of every
// series (name+labels) in metricFamilies, keyed by its full "name{labels}"
// identifier. Histograms are captured via their _sum/_count series.
func scrapeMetrics(ctx context.Context, kube *kubernetes.Clientset) (map[string]float64, error) {
	raw, err := kube.Discovery().RESTClient().Get().AbsPath("/metrics").DoRaw(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]float64{}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		// Keep only the families we care about, and for histograms only the
		// _sum/_count series (skip the many _bucket lines).
		name := line
		if i := strings.IndexAny(line, "{ "); i >= 0 {
			name = line[:i]
		}
		wanted := false
		for _, f := range metricFamilies {
			if name == f || name == f+"_sum" || name == f+"_count" {
				wanted = true
				break
			}
		}
		if !wanted {
			continue
		}
		sp := strings.LastIndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(line[sp+1:]), 64)
		if err != nil {
			continue
		}
		out[line[:sp]] = v
	}
	return out, sc.Err()
}

// printMetricDiff reports, per admission series that did work during the fill,
// how many admissions ran and their server-measured mean duration, plus the
// apiserver CPU consumed. Only series whose count increased are shown, so on a
// quiet cluster this is essentially just this test's workload.
func printMetricDiff(before, after map[string]float64, fill int) {
	fmt.Printf("\nserver-side cost during the fill (%d writes, from apiserver /metrics):\n", fill)

	// CPU consumed by the apiserver process over the fill.
	for k, av := range after {
		if strings.HasPrefix(k, "process_cpu_seconds_total") {
			if bv, ok := before[k]; ok && av-bv > 0 {
				fmt.Printf("  apiserver CPU consumed: +%.3fs\n", av-bv)
			}
		}
	}

	// Per-admission mean = Δsum/Δcount for each histogram series that advanced.
	type row struct {
		series string
		count  float64
		mean   time.Duration
	}
	var rows []row
	for k, av := range after {
		if !strings.Contains(k, "_duration_seconds_count") {
			continue
		}
		dCount := av - before[k]
		if dCount <= 0 {
			continue
		}
		// apiserver_request_duration_seconds covers every resource; keep only our
		// ipslices writes so the total-server-time rows aren't drowned out.
		isRequest := strings.HasPrefix(k, "apiserver_request_duration_seconds")
		if isRequest && !strings.Contains(k, `resource="ipslices"`) {
			continue
		}
		sumKey := strings.Replace(k, "_duration_seconds_count", "_duration_seconds_sum", 1)
		dSum := after[sumKey] - before[sumKey]
		mean := time.Duration(dSum / dCount * float64(time.Second))
		// Drop the ~0-1µs built-in plugins; always keep ipslices request rows.
		if mean < noiseFloor && !isRequest {
			continue
		}
		series := strings.TrimSuffix(k, "_count")
		series = strings.Replace(series, "_duration_seconds", "", 1)
		rows = append(rows, row{series: series, count: dCount, mean: mean})
	}
	if len(rows) == 0 {
		fmt.Println("  (no series advanced — check /metrics access)")
		return
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].mean > rows[j].mean })
	for _, r := range rows {
		fmt.Printf("  %-78s %4.0f x, mean %v\n", r.series, r.count, round(r.mean))
	}
	fmt.Println("  note: CEL admission = MutatingAdmissionPolicy (allocator) + ValidatingAdmissionPolicy (VAP).")
	fmt.Println("        apiserver_request(ipslices) minus admission_step ~= CRD x-kubernetes-validations + etcd.")
}

// newSlice builds a valid, server-named /26 IPSlice at the given network address.
func newSlice(networkAddress int64) *v1alpha1.IPSlice {
	spec := v1alpha1.IPSliceSpec{
		PodNetworkRef: v1alpha1.PodNetworkRef{Kind: "blue-network", Name: "abc"},
		SliceSubnet:   v1alpha1.Subnet{NetworkAddress: networkAddress, PrefixLength: 26, AddressSpace: 64},
	}
	return &v1alpha1.IPSlice{
		ObjectMeta: metav1.ObjectMeta{Name: naming.Name(spec)},
		Spec:       spec,
	}
}

func printStats(label string, ds []time.Duration) {
	if len(ds) == 0 {
		fmt.Printf("%s: no samples\n", label)
		return
	}
	sorted := append([]time.Duration(nil), ds...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	fmt.Printf("%s (n=%d):\n", label, len(ds))
	fmt.Printf("  min %v  p50 %v  p90 %v  max %v  mean %v\n",
		round(sorted[0]), round(pctile(sorted, 0.50)), round(pctile(sorted, 0.90)),
		round(sorted[len(sorted)-1]), round(mean(ds)))
}

func mean(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	var sum time.Duration
	for _, d := range ds {
		sum += d
	}
	return sum / time.Duration(len(ds))
}

// pctile expects a pre-sorted slice.
func pctile(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(q * float64(len(sorted)-1))
	return sorted[idx]
}

func pct(from, to time.Duration) float64 {
	if from == 0 {
		return 0
	}
	return (float64(to) - float64(from)) / float64(from) * 100
}

func round(d time.Duration) time.Duration { return d.Round(time.Microsecond) }
