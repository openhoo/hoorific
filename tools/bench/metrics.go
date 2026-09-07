package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type caseResult struct {
	Target                   string           `json:"target"`
	Workload                 string           `json:"workload"`
	Run                      int              `json:"run"`
	Concurrency              int              `json:"concurrency"`
	InputBytes               int              `json:"input_bytes"`
	Stream                   bool             `json:"stream"`
	Attempted                int64            `json:"attempted"`
	Completed                int64            `json:"completed"`
	Errors                   int64            `json:"errors"`
	Dropped                  int64            `json:"dropped"`
	Rejected                 int64            `json:"rejected"`
	WarmupAttempted          int64            `json:"warmup_attempted"`
	WarmupErrors             int64            `json:"warmup_errors"`
	WarmupDropped            int64            `json:"warmup_dropped"`
	OfferedRate              float64          `json:"offered_rate_rps"`
	AcceptedRate             float64          `json:"accepted_rate_rps"`
	CompletedRate            float64          `json:"completed_rate_rps"`
	RejectedRate             float64          `json:"rejected_rate_rps"`
	TotalMs, TTFTMs, QueueMs []float64        `json:"-"`
	TotalP50                 float64          `json:"total_p50_ms"`
	TotalP95                 float64          `json:"total_p95_ms"`
	TotalP99                 float64          `json:"total_p99_ms"`
	TTFTP50                  float64          `json:"ttft_p50_ms"`
	TTFTP95                  float64          `json:"ttft_p95_ms"`
	TTFTP99                  float64          `json:"ttft_p99_ms"`
	QueueP50                 float64          `json:"queue_p50_ms"`
	QueueP95                 float64          `json:"queue_p95_ms"`
	QueueP99                 float64          `json:"queue_p99_ms"`
	DurationSeconds          float64          `json:"duration_seconds"`
	AllocationMetric         string           `json:"allocation_metric"`
	ErrorCounts              map[string]int64 `json:"error_counts,omitempty"`
	RSSAvailable             bool             `json:"rss_available"`
	RSSBytes                 []int64          `json:"rss_bytes,omitempty"`
	OpenStreams              int64            `json:"open_streams,omitempty"`
	CancellationMs           float64          `json:"cancellation_ms,omitempty"`
	ClientCloseMs            float64          `json:"client_close_ms,omitempty"`
	UpstreamCloseObserved    bool             `json:"upstream_close_observed"`
	ActiveStallsAtEnd        int64            `json:"active_stalls_at_measurement_end,omitempty"`
	ActiveStallsAfterCancel  int64            `json:"active_stalls_after_cancel,omitempty"`
}

// Percentile samples stay in memory for the current report and are never substituted with runner metrics.

type report struct {
	SchemaVersion           int               `json:"schema_version"`
	State                   string            `json:"state"`
	Issues                  []string          `json:"issues"`
	Environment             reportEnvironment `json:"environment"`
	BinaryBytes             int64             `json:"binary_bytes"`
	BinaryBytesStatus       string            `json:"binary_bytes_status"`
	RunnerBinaryBytes       int64             `json:"runner_binary_bytes"`
	RunnerBinaryBytesStatus string            `json:"runner_binary_bytes_status"`
	ExpectedResults         int               `json:"expected_results"`
	Results                 []caseResult      `json:"results"`
	Comparison              string            `json:"comparison"`
}
type reportEnvironment struct {
	TimestampUTC, GOOS, GOARCH, GoVersion string
	CPUCount, GOMAXPROCS int
	Hostname, HostnameStatus, Deployment, Route, AnnotationSource, BinaryPath, BinaryPathStatus string
	GatewayPID int
	DurationNS, WarmupNS, RequestTimeoutNS, SessionTimeoutNS int64
	Transport transportFacts `json:"transport"`
}

func newReport(o options) report {
	host, hs := "", "unavailable"
	if h, e := os.Hostname(); e == nil {
		host, hs = safeArtifactText(h), "available"
	}
	return report{1, "unmet_prerequisite_or_incomplete", []string{}, reportEnvironment{time.Now().UTC().Format(time.RFC3339Nano), runtime.GOOS, runtime.GOARCH, runtime.Version(), runtime.NumCPU(), runtime.GOMAXPROCS(0), host, hs, o.mode, o.route, "operator supplied; not independently detected", safeArtifactText(o.binary), "operator supplied", o.pid, int64(o.duration), int64(o.warmup), int64(o.requestTimeout), int64(o.sessionTimeout), transportFacts{Profile: o.profile}}, 0, "unavailable: not inventoried", 0, "unavailable: not inventoried", 3 * 2 * len(workloads()), nil, "No performance comparison claims are made."}
}
func safeArtifactText(v string) string {
	for _, n := range []string{"BENCH_GATEWAY_KEY", "BENCH_GATEWAY_URL"} {
		if s := strings.TrimSpace(os.Getenv(n)); s != "" {
			v = strings.ReplaceAll(v, s, "[redacted]")
		}
	}
	if strings.Contains(v, "://") || strings.ContainsAny(v, "\r\n\x00") {
		return "[redacted unsafe text]"
	}
	return v
}
func rssSample(pid int) (int64, bool) {
	if pid <= 0 {
		return 0, false
	}
	f, e := os.Open(fmt.Sprintf("/proc/%d/status", pid))
	if e != nil {
		return 0, false
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		p := strings.Fields(s.Text())
		if len(p) >= 2 && p[0] == "VmRSS:" {
			n, e := strconv.ParseInt(p[1], 10, 64)
			return n * 1024, e == nil
		}
	}
	return 0, false
}
func sampleProcess(pid int, result *caseResult, mu *sync.Mutex) func() {
	if pid <= 0 {
		return func() {}
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if rss, ok := rssSample(pid); ok {
					mu.Lock()
					result.RSSAvailable = true
					result.RSSBytes = append(result.RSSBytes, rss)
					mu.Unlock()
				}
			case <-stop:
				return
			}
		}
	}()
	return func() {
		close(stop)
		<-done
		if rss, ok := rssSample(pid); ok {
			mu.Lock()
			result.RSSAvailable = true
			result.RSSBytes = append(result.RSSBytes, rss)
			mu.Unlock()
		}
	}
}
func percentile(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	x := append([]float64(nil), v...)
	sort.Float64s(x)
	k := (p / 100) * float64(len(x)-1)
	lo := int(k)
	hi := lo + 1
	if hi >= len(x) {
		return x[lo]
	}
	return x[lo] + (x[hi]-x[lo])*(k-float64(lo))
}
func saveReport(dir string, r report) error {
	for i := range r.Results {
		c := &r.Results[i]
		c.TotalP50 = percentile(c.TotalMs, 50)
		c.TotalP95 = percentile(c.TotalMs, 95)
		c.TotalP99 = percentile(c.TotalMs, 99)
		c.TTFTP50 = percentile(c.TTFTMs, 50)
		c.TTFTP95 = percentile(c.TTFTMs, 95)
		c.TTFTP99 = percentile(c.TTFTMs, 99)
		c.QueueP50 = percentile(c.QueueMs, 50)
		c.QueueP95 = percentile(c.QueueMs, 95)
		c.QueueP99 = percentile(c.QueueMs, 99)
		if c.AllocationMetric == "" {
			c.AllocationMetric = "unavailable: gateway allocation runtime metric not exposed"
		}
	}
	b, e := json.MarshalIndent(r, "", "  ")
	if e != nil {
		return e
	}
	if e = atomicWrite(filepath.Join(dir, "results.json"), append(b, '\n')); e != nil {
		return e
	}
	return atomicWrite(filepath.Join(dir, "summary.txt"), []byte(summary(r)))
}
func atomicWrite(path string, b []byte) error {
	tmp := path + fmt.Sprintf(".tmp-%d", os.Getpid())
	if e := os.WriteFile(tmp, b, 0600); e != nil {
		return e
	}
	if e := os.Rename(tmp, path); e != nil {
		_ = os.Remove(tmp)
		return e
	}
	return nil
}
func summary(r report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "state: %s\nexpected_results: %d\n", r.State, r.ExpectedResults)
	if len(r.Issues) > 0 {
		fmt.Fprintf(&b, "issues: %s\n", strings.Join(r.Issues, "; "))
	}
	fmt.Fprintf(&b, "deployment: %s route: %s\ncomparison: %s\nallocations: unavailable (external gateway runtime metric not exposed)\n", r.Environment.Deployment, r.Environment.Route, r.Comparison)
	fmt.Fprintf(&b, "transport_profile: %s proof_status: %s direct_trust: %s\n", r.Environment.Transport.Profile, r.Environment.Transport.ProofStatus, r.Environment.Transport.TrustStatus)
	for name, link := range r.Environment.Transport.Links {
		fmt.Fprintf(&b, "transport_link[%s]: %s got_conn=%d reused=%d tls_versions=%v\n", name, link.Status, link.Connections, link.Reused, link.TLSVersions)
	}
	for _, c := range r.Results {
		fmt.Fprintf(&b, "%s %s run=%d attempted=%d completed=%d errors=%d dropped=%d rejected=%d offered=%.2f achieved=%.2f total_ms(p50/p95/p99)=%.3f/%.3f/%.3f ttft_ms=%.3f/%.3f/%.3f rss=%s\n", c.Target, c.Workload, c.Run, c.Attempted, c.Completed, c.Errors, c.Dropped, c.Rejected, c.OfferedRate, c.CompletedRate, c.TotalP50, c.TotalP95, c.TotalP99, c.TTFTP50, c.TTFTP95, c.TTFTP99, func() string {
			if c.RSSAvailable {
				return "available"
			}
			return "unavailable"
		}())
	}
	return b.String()
}
