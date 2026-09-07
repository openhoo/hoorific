// Command bench measures a provisioned gateway against a deterministic loopback upstream.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"
)
type options struct {
	binary, output, urlFile, keyFile, model, listen, mode, route, profile, proofFile string
	duration, warmup, requestTimeout, sessionTimeout                         time.Duration
	pid                                                                      int
	suite                                                                    bool
}

func main() { os.Exit(run()) }

func run() int {
	var o options
	flag.StringVar(&o.binary, "binary", ".artifacts/hoorific", "gateway binary to inventory; never launched or migrated")
	flag.StringVar(&o.output, "output", ".artifacts/bench", "artifact directory (private; results.json and summary.txt replaced)")
	flag.StringVar(&o.urlFile, "url-file", "", "file containing full gateway chat completions URL; otherwise BENCH_GATEWAY_URL")
	flag.StringVar(&o.keyFile, "key-file", "", "private API key file; otherwise BENCH_GATEWAY_KEY")
	flag.StringVar(&o.model, "model", "bench-fixture", "provisioned gateway model alias")
	flag.StringVar(&o.listen, "fixture-listen", "127.0.0.1:18089", "loopback fixture address; configure gateway upstream http://ADDRESS/v1")
	flag.StringVar(&o.mode, "deployment", "unspecified", "operator annotation: standalone or cluster")
	flag.StringVar(&o.route, "route", "unspecified", "operator annotation: native or translated (not auto-detected)")
	flag.StringVar(&o.profile, "upstream-transport", "http", "matched upstream profile: http or tls")
	flag.StringVar(&o.proofFile, "transport-proof-file", "", "suite or operator transport proof JSON")
	flag.IntVar(&o.pid, "gateway-pid", 0, "local gateway PID for /proc RSS; zero marks RSS unavailable")
	flag.DurationVar(&o.duration, "duration", 30*time.Second, "measurement arrival window per case/run (1s to 5m); workloads are fixed")
	flag.DurationVar(&o.warmup, "warmup", 5*time.Second, "separate warmup per case/run (at least 1s)")
	flag.DurationVar(&o.requestTimeout, "request-timeout", 15*time.Second, "per-request timeout including slow consumption (1s to 1m)")
	flag.DurationVar(&o.sessionTimeout, "session-timeout", 2*time.Hour, "whole-session deadline (at most 24h)")
	flag.BoolVar(&o.suite, "suite", true, "provision isolated SQLite/PostgreSQL/Redis and run the complete benchmark matrix; pass --suite=false for manual gateway mode")
	flag.Parse()
	report := newReport(o)
	if o.suite {
		suiteCtx, suiteCancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer suiteCancel()
		if err := runSuite(suiteCtx, o.binary, o.output, o.profile); err != nil {
			fmt.Fprintln(os.Stderr, "benchmark suite failed:", err)
			return 1
		}
		return 0
	}
	if o.profile != "http" && o.profile != "tls" {
		fmt.Fprintln(os.Stderr, "invalid --upstream-transport: must be http or tls")
		return 1
	}
	if err := os.MkdirAll(o.output, 0700); err != nil {
		fmt.Fprintln(os.Stderr, "cannot create private artifact directory")
		return 1
	}
	if err := claimProfile(o.output, o.profile); err != nil {
		fmt.Fprintln(os.Stderr, "cannot claim benchmark output:", err)
		return 1
	}
	fail := func(message string) int {
		report.State = "unmet_prerequisite_or_incomplete"
		report.Issues = append(report.Issues, message)
		if err := saveReport(o.output, report); err != nil {
			fmt.Fprintln(os.Stderr, "cannot write benchmark artifacts")
		}
		fmt.Fprintln(os.Stderr, message)
		return 1
	}
	if flag.NArg() != 0 || o.duration < time.Second || o.duration > 5*time.Minute || o.warmup < time.Second || o.warmup > time.Minute || o.requestTimeout < time.Second || o.requestTimeout > time.Minute || o.sessionTimeout <= 0 || o.sessionTimeout > 24*time.Hour || o.pid < 0 {
		return fail("invalid flags: see --help; fixed rates/concurrency and three runs cannot be reduced")
	}
	if o.mode != "standalone" && o.mode != "cluster" {
		return fail("--deployment must identify standalone or cluster")
	}
	if o.route != "native" && o.route != "translated" {
		return fail("--route must identify native or translated")
	}
	size, err := os.Stat(o.binary)
	if err != nil || !size.Mode().IsRegular() {
		return fail("--binary must identify an existing gateway binary; build separately")
	}
	report.BinaryBytes = size.Size()
	report.BinaryBytesStatus = "available"
	if executable, err := os.Executable(); err == nil {
		if st, err := os.Stat(executable); err == nil {
			report.RunnerBinaryBytes = st.Size()
			report.RunnerBinaryBytesStatus = "available"
		}
	}
	endpoint, err := secretValue(o.urlFile, "BENCH_GATEWAY_URL", false)
	if err != nil {
		return fail("gateway URL unavailable: set BENCH_GATEWAY_URL or --url-file")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fail("gateway URL must be an absolute HTTP(S) endpoint without userinfo, query, or fragment")
	}
	key, err := secretValue(o.keyFile, "BENCH_GATEWAY_KEY", true)
	if err != nil || strings.ContainsAny(key, "\r\n") {
		return fail("API key unavailable or invalid: use a private --key-file or BENCH_GATEWAY_KEY")
	}
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(signalCtx, o.sessionTimeout)
	defer cancel()
	fixture, closeFixture, err := startFixture(ctx, o.listen)
	if err != nil {
		return fail("cannot start loopback fixture; verify --fixture-listen is a free loopback address")
	}
	defer closeFixture()
	transport, err := loadChildTransport(o, endpoint, fixture)
	if err != nil {
		return fail("transport proof invalid: " + err.Error())
	}
	report.Environment.Transport = transport.snapshot()
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fail("cannot generate fixture challenge")
	}
	prefix := "BENCH:" + hex.EncodeToString(nonce[:]) + ":"
	targets := []target{{Name: "direct", URL: transport.proof.DirectEndpoint, Model: o.model}, {Name: "gateway", URL: endpoint, Key: key, Model: o.model}}
	for _, t := range targets {
		client, closeClient := transport.client(t, o.requestTimeout)
		for _, stream := range []bool{false, true} {
			w := workload{Name: "preflight", InputBytes: 1024, Stream: stream, Concurrency: 1}
			sample := request(ctx, client, t, w, prefix, time.Now(), o.requestTimeout)
			if sample.Error != "" {
				closeClient()
				return fail(fmt.Sprintf("%s fixture preflight failed (%s; stream=%t; %s); inspect isolated gateway/fixture contract", t.Name, sample.Error, stream, sample.ErrorDetail))
			}
		}
		closeClient()
	}
	report.Environment.Transport = transport.snapshot()
	report.State = "running"
	if err := saveReport(o.output, report); err != nil {
		return fail("cannot write initial artifacts")
	}
	for run := 1; run <= 3; run++ {
		for _, w := range workloads() {
			// Alternate target order to expose, not hide, ordering bias.
			order := targets
			if run%2 == 0 {
				order = []target{targets[1], targets[0]}
			}
			for _, t := range order {
				if ctx.Err() != nil {
					return fail("session interrupted or deadline reached; completed cases retained")
				}
				client, closeClient := transport.client(t, o.requestTimeout)
				warm := measure(ctx, client, t, w, prefix, o.warmup, o.requestTimeout, 0)
				result := measure(ctx, client, t, w, prefix, o.duration, o.requestTimeout, o.pidFor(t))
				closeClient()
				result.Run = run
				result.WarmupAttempted = warm.Attempted
				result.WarmupErrors = warm.Errors
				result.WarmupDropped = warm.Dropped
				report.Results = append(report.Results, result)
				report.Environment.Transport = transport.snapshot()
				if err := saveReport(o.output, report); err != nil {
					return fail("cannot persist case artifacts")
				}
			}
		}
	}
	if ctx.Err() != nil {
		return fail("session interrupted or deadline reached; results incomplete")
	}
	report.State = "complete"
	for _, r := range report.Results {
		if r.Errors > 0 || r.Dropped > 0 || r.WarmupErrors > 0 || r.WarmupDropped > 0 {
			report.State = "complete_with_failures"
		}
	}
	report.Environment.Transport = transport.snapshot()
	if err := saveReport(o.output, report); err != nil {
		return fail("cannot write final artifacts")
	}
	fmt.Println("Benchmark artifacts written; see results.json and summary.txt in the output directory.")
	if report.State != "complete" {
		return 1
	}
	return 0
}

func (o options) pidFor(t target) int {
	if t.Name == "gateway" {
		return o.pid
	}
	return 0
}

func secretValue(path, name string, private bool) (string, error) {
	value := os.Getenv(name)
	if path != "" {
		st, err := os.Stat(path)
		if err != nil || !st.Mode().IsRegular() || st.Size() > 64*1024 {
			return "", fmt.Errorf("invalid secret file")
		}
		if private && runtime.GOOS != "windows" && st.Mode().Perm()&0077 != 0 {
			return "", fmt.Errorf("key file is not private")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("secret read failed")
		}
		value = string(data)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("missing value")
	}
	return value, nil
}

func newClient(timeout time.Duration) (*http.Client, func()) {
	tr := &http.Transport{Proxy: nil, MaxIdleConns: 2048, MaxIdleConnsPerHost: 1024, MaxConnsPerHost: 0, IdleConnTimeout: 30 * time.Second, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: timeout, DisableCompression: true}
	client := &http.Client{Transport: tr, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return client, tr.CloseIdleConnections
}
