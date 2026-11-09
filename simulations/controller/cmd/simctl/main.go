// Command simctl drives the simulation API from test runners and shells.
//
//	export SIMCTL_SERVER=https://sim-hub.example:8443 SIMCTL_TOKEN=...
//	simctl catalog
//	simctl --cluster lab-2 inject gpu-xid --graph mocker-agg --component decode \
//	    --param xid=79 --duration 2m --label testrun=nightly --wait recovered
//	simctl --cluster lab-2 list --selector testrun=nightly
//	simctl recover-all --fleet --selector testrun=nightly
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	simv1 "github.com/benmaguire/onit-ai-testenv/simulations/controller/api/v1alpha1"
	"github.com/benmaguire/onit-ai-testenv/simulations/controller/pkg/catalog"
	apic "github.com/benmaguire/onit-ai-testenv/simulations/controller/pkg/client"
)

const usage = `simctl — manage failure/recovery simulations

Usage: simctl [global flags] <command> [flags]

Commands:
  version                      server (or routed agent) version
  catalog [type]               available simulation types
  clusters                     clusters registered with a hub
  list                         list simulations (--selector, --phase)
  get <name>                   show one simulation
  inject <type>                create a simulation (see 'simctl inject -h')
  recover <name> [--wait]      recover a simulation now
  delete <name>                delete a simulation (its fault is reverted first)
  wait <name> --for <phase>    wait for Active, Recovered, ...
  recover-all [--fleet]        recover every active simulation (emergency stop)
  fleet                        list simulations on every cluster of a hub
  report <name> [--wait]       baseline / fault / recovery / after measurements
  metrics <name> [--step 30s]  raw series + probe samples as CSV (stdout)
  profiles                     metrics profiles available for --observe

Global flags (or env SIMCTL_SERVER, SIMCTL_TOKEN, SIMCTL_CLUSTER, SIMCTL_NAMESPACE):
`

type kvFlag map[string]string

func (k kvFlag) String() string { return "" }
func (k kvFlag) Set(s string) error {
	for _, kv := range strings.Split(s, ",") {
		key, val, ok := strings.Cut(kv, "=")
		if !ok || key == "" {
			return fmt.Errorf("%q is not key=value", kv)
		}
		k[strings.TrimSpace(key)] = strings.TrimSpace(val)
	}
	return nil
}

var (
	out = "table"
	ns  string
)

func main() {
	g := flag.NewFlagSet("simctl", flag.ExitOnError)
	server := g.String("server", env("SIMCTL_SERVER", "http://localhost:8443"), "API base URL (agent or hub)")
	token := g.String("token", os.Getenv("SIMCTL_TOKEN"), "bearer token")
	cluster := g.String("cluster", os.Getenv("SIMCTL_CLUSTER"), "route through the hub to this cluster")
	insecure := g.Bool("insecure", false, "skip TLS verification")
	g.StringVar(&ns, "n", env("SIMCTL_NAMESPACE", ""), "namespace (default: server default)")
	g.StringVar(&out, "o", "table", "output: table or json")
	g.Usage = func() { fmt.Fprint(os.Stderr, usage); g.PrintDefaults() }
	_ = g.Parse(os.Args[1:])
	if g.NArg() == 0 {
		g.Usage()
		os.Exit(2)
	}
	c := apic.New(*server, *token)
	c.Cluster = *cluster
	if *insecure {
		c.Insecure()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := dispatch(ctx, c, g.Arg(0), g.Args()[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func dispatch(ctx context.Context, c *apic.Client, cmd string, args []string) error {
	switch cmd {
	case "version":
		v, err := c.Version(ctx)
		if err != nil {
			return err
		}
		return printJSON(v)
	case "catalog":
		return cmdCatalog(ctx, c, args)
	case "clusters":
		return cmdClusters(ctx, c)
	case "list", "ls":
		return cmdList(ctx, c, args)
	case "get":
		name, err := oneArg(args, "get <name>")
		if err != nil {
			return err
		}
		s, err := c.Get(ctx, defaultNS(), name)
		if err != nil {
			return err
		}
		return printSims([]simv1.Simulation{*s}, true)
	case "inject", "create":
		return cmdInject(ctx, c, args)
	case "recover":
		return cmdRecover(ctx, c, args)
	case "delete", "rm":
		name, err := oneArg(args, "delete <name>")
		if err != nil {
			return err
		}
		if err := c.Delete(ctx, defaultNS(), name); err != nil {
			return err
		}
		fmt.Println("deleted", name)
		return nil
	case "wait":
		return cmdWait(ctx, c, args)
	case "recover-all":
		return cmdRecoverAll(ctx, c, args)
	case "fleet":
		return cmdFleet(ctx, c, args)
	case "report":
		return cmdReport(ctx, c, args)
	case "metrics":
		return cmdMetrics(ctx, c, args)
	case "profiles":
		return cmdProfiles(ctx, c)
	}
	return fmt.Errorf("unknown command %q (simctl -h)", cmd)
}

// defaultNS: "" lets the server pick for create; reads need a namespace.
func defaultNS() string {
	if ns != "" {
		return ns
	}
	return "sim-system"
}

func cmdCatalog(ctx context.Context, c *apic.Client, args []string) error {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	if len(args) == 1 {
		for _, e := range cat {
			if e.Type == args[0] {
				return printEntry(e)
			}
		}
		return fmt.Errorf("no simulation type %q", args[0])
	}
	if out == "json" {
		return printJSON(cat)
	}
	w := tab()
	fmt.Fprintln(w, "TYPE\tCATEGORY\tSUMMARY")
	for _, e := range cat {
		fmt.Fprintf(w, "%s\t%s\t%s\n", e.Type, e.Category, e.Summary)
	}
	return w.Flush()
}

func printEntry(e catalog.Entry) error {
	if out == "json" {
		return printJSON(e)
	}
	fmt.Printf("%s (%s)\n  %s\n\nEffect:   %s\nRecovery: %s\nTargets:  %s\n", e.Type, e.Category, e.Summary, e.Effect, e.Recovery, strings.Join(e.Targets, ", "))
	if len(e.Params) > 0 {
		fmt.Println("Params:")
		for _, p := range e.Params {
			def := ""
			if p.Default != "" {
				def = " (default " + p.Default + ")"
			}
			if p.Required {
				def += " [required]"
			}
			fmt.Printf("  %-18s %s%s\n", p.Name, p.Description, def)
		}
	}
	for _, r := range e.Requires {
		fmt.Println("Requires:", r)
	}
	return nil
}

func cmdClusters(ctx context.Context, c *apic.Client) error {
	cl, err := c.Clusters(ctx)
	if err != nil {
		return err
	}
	if out == "json" {
		return printJSON(cl)
	}
	w := tab()
	fmt.Fprintln(w, "NAME\tENV\tREACHABLE\tACTIVE\tVERSION\tENDPOINT\tMESSAGE")
	for _, x := range cl {
		name := x.Name
		if x.Local {
			name += " (local)"
		}
		fmt.Fprintf(w, "%s\t%s\t%v\t%d\t%s\t%s\t%s\n", name, x.Environment, x.Reachable, x.ActiveSimulations, x.AgentVersion, x.Endpoint, x.Message)
	}
	return w.Flush()
}

func cmdList(ctx context.Context, c *apic.Client, args []string) error {
	f := flag.NewFlagSet("list", flag.ExitOnError)
	sel := f.String("selector", "", "label selector, e.g. testrun=nightly")
	phase := f.String("phase", "", "only this phase")
	all := f.Bool("A", false, "all namespaces")
	_ = f.Parse(args)
	n := ns
	if *all {
		n = ""
	}
	sims, err := c.List(ctx, apic.ListOptions{Namespace: n, LabelSelector: *sel, Phase: simv1.Phase(*phase)})
	if err != nil {
		return err
	}
	return printSims(sims, false)
}

func cmdInject(ctx context.Context, c *apic.Client, args []string) error {
	f := flag.NewFlagSet("inject", flag.ExitOnError)
	name := f.String("name", "", "simulation name (default generated)")
	node := f.String("node", "", "target node")
	gpu := f.String("gpu", "", "target GPU index, all, or UUID")
	graph := f.String("graph", "", "target DynamoGraphDeployment")
	comp := f.String("component", "", "target Dynamo component (frontend, decode, prefill)")
	tns := f.String("target-namespace", "", "namespace of target pods")
	count := f.Int("count", 0, "max number of targets (0 = all)")
	dur := f.Duration("duration", 0, "auto-recover after this long (0 = hold until recovered)")
	delay := f.Duration("delay", 0, "wait before injecting")
	rto := f.Duration("recovery-timeout", 0, "max time for recovery verification (default 5m)")
	ttl := f.Duration("ttl", 0, "delete this long after finishing")
	wait := f.String("wait", "", "wait for a phase: active or recovered; 'observed' also waits for the after window")
	baseline := f.Duration("baseline", 0, "observe: baseline window before the fault")
	after := f.Duration("after", 0, "observe: window after recovery")
	obsStep := f.Duration("observe-step", 0, "observe: Prometheus range-query step (default 15s)")
	profiles := &listFlag{}
	f.Var(profiles, "observe", "observe: metrics profile (repeatable): kubernetes, gpu, dynamo, triton, kai, network, probes")
	queries := kvFlag{}
	f.Var(queries, "query", "observe: extra PromQL name=query (repeatable)")
	probeHTTP := &listFlag{}
	f.Var(probeHTTP, "probe-http", "observe: HTTP probe URL, optionally name=URL (repeatable)")
	probeTCP := &listFlag{}
	f.Var(probeTCP, "probe-tcp", "observe: TCP probe host:port, optionally name=host:port (repeatable)")
	probeExec := &listFlag{}
	f.Var(probeExec, "probe-exec", "observe: exec probe <namespace>/<pod or label=value>:<command> (repeatable), e.g. xrd-lab/app=host1:ping -c1 -W1 192.168.2.10")
	probeInterval := f.Duration("probe-interval", 2*time.Second, "observe: probe interval")
	timeout := f.Duration("timeout", 15*time.Minute, "max time for --wait")
	params, podSel, nodeSel, labels := kvFlag{}, kvFlag{}, kvFlag{}, kvFlag{}
	f.Var(params, "param", "type parameter key=value (repeatable)")
	f.Var(podSel, "pod-selector", "target pod labels key=value (repeatable)")
	f.Var(nodeSel, "node-selector", "target node labels key=value (repeatable)")
	f.Var(labels, "label", "label for the simulation key=value (repeatable)")
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		f.Usage = func() {
			fmt.Fprintln(os.Stderr, "Usage: simctl inject <type> [flags]   (types: simctl catalog)")
			f.PrintDefaults()
		}
		_ = f.Parse(args)
		f.Usage()
		return fmt.Errorf("missing simulation type")
	}
	typ := args[0]
	_ = f.Parse(args[1:])

	spec := simv1.SimulationSpec{Type: typ, Params: params, Target: simv1.Target{
		Node: *node, GPU: *gpu, Graph: *graph, Component: *comp, Namespace: *tns,
		Count: int32(*count), PodSelector: nilIfEmpty(podSel), NodeSelector: nilIfEmpty(nodeSel),
	}}
	spec.Params = nilIfEmpty(params)
	spec.Duration = dptr(*dur)
	spec.Delay = dptr(*delay)
	spec.RecoveryTimeout = dptr(*rto)
	spec.TTLAfterFinished = dptr(*ttl)
	if *baseline > 0 || *after > 0 || len(*profiles) > 0 || len(queries) > 0 || len(*probeHTTP) > 0 || len(*probeTCP) > 0 || len(*probeExec) > 0 {
		ob := &simv1.ObserveSpec{Baseline: dptr(*baseline), After: dptr(*after), Profiles: *profiles, Step: dptr(*obsStep)}
		names := make([]string, 0, len(queries))
		for n := range queries {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			ob.Queries = append(ob.Queries, simv1.MetricQuery{Name: n, Query: queries[n]})
		}
		for i, v := range *probeHTTP {
			name, u := splitName(v, fmt.Sprintf("http-%d", i+1), "http")
			ob.Probes = append(ob.Probes, simv1.Probe{Name: name, Type: "http", URL: u, Interval: dptr(*probeInterval)})
		}
		for i, v := range *probeTCP {
			name, a := splitName(v, fmt.Sprintf("tcp-%d", i+1), "")
			ob.Probes = append(ob.Probes, simv1.Probe{Name: name, Type: "tcp", Address: a, Interval: dptr(*probeInterval)})
		}
		for i, v := range *probeExec {
			ep, err := parseExecProbe(v)
			if err != nil {
				return err
			}
			ob.Probes = append(ob.Probes, simv1.Probe{Name: fmt.Sprintf("exec-%d", i+1), Type: "exec", Exec: ep, Interval: dptr(*probeInterval)})
		}
		spec.Observe = ob
	}
	s, err := c.Create(ctx, apic.CreateRequest{Name: *name, Namespace: ns, Labels: nilIfEmpty(labels), Spec: spec})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "created %s/%s\n", s.Namespace, s.Name)
	if *wait == "observed" {
		wctx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		if _, err := c.WaitObserved(wctx, s.Namespace, s.Name); err != nil {
			return err
		}
		return printReport(ctx, c, s.Namespace, s.Name)
	}
	if *wait != "" {
		return waitPhase(ctx, c, s.Namespace, s.Name, *wait, *timeout)
	}
	if out == "json" {
		return printJSON(s)
	}
	fmt.Println(s.Name)
	return nil
}

func cmdRecover(ctx context.Context, c *apic.Client, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: recover <name> [--wait] [--timeout 10m]")
	}
	f := flag.NewFlagSet("recover", flag.ExitOnError)
	wait := f.Bool("wait", false, "wait until Recovered")
	timeout := f.Duration("timeout", 10*time.Minute, "max wait")
	_ = f.Parse(args[1:])
	if _, err := c.Recover(ctx, defaultNS(), args[0]); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "recovery requested for", args[0])
	if *wait {
		return waitPhase(ctx, c, defaultNS(), args[0], "recovered", *timeout)
	}
	return nil
}

func cmdWait(ctx context.Context, c *apic.Client, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: wait <name> --for <active|recovered> [--timeout 10m]")
	}
	f := flag.NewFlagSet("wait", flag.ExitOnError)
	phase := f.String("for", "recovered", "phase to wait for")
	timeout := f.Duration("timeout", 10*time.Minute, "max wait")
	_ = f.Parse(args[1:])
	return waitPhase(ctx, c, defaultNS(), args[0], *phase, *timeout)
}

func waitPhase(ctx context.Context, c *apic.Client, namespace, name, phase string, timeout time.Duration) error {
	p := simv1.Phase(strings.ToUpper(phase[:1]) + strings.ToLower(phase[1:]))
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	s, err := c.WaitFor(ctx, namespace, name, p)
	if s != nil {
		_ = printSims([]simv1.Simulation{*s}, true)
	}
	return err
}

func cmdRecoverAll(ctx context.Context, c *apic.Client, args []string) error {
	f := flag.NewFlagSet("recover-all", flag.ExitOnError)
	sel := f.String("selector", "", "only simulations with these labels")
	fleet := f.Bool("fleet", false, "on every cluster a hub manages")
	_ = f.Parse(args)
	if *fleet {
		res, err := c.FleetRecoverAll(ctx, *sel)
		if err != nil {
			return err
		}
		return printJSON(res)
	}
	res, err := c.RecoverAll(ctx, ns, *sel)
	if err != nil {
		return err
	}
	return printJSON(res)
}

func cmdFleet(ctx context.Context, c *apic.Client, args []string) error {
	f := flag.NewFlagSet("fleet", flag.ExitOnError)
	sel := f.String("selector", "", "label selector")
	_ = f.Parse(args)
	res, err := c.FleetSimulations(ctx, *sel)
	if err != nil {
		return err
	}
	if out == "json" {
		return printJSON(res)
	}
	w := tab()
	fmt.Fprintln(w, "CLUSTER\tNAMESPACE\tNAME\tTYPE\tPHASE\tMESSAGE")
	for _, r := range res {
		if r.Error != "" {
			fmt.Fprintf(w, "%s\t-\t-\t-\tERROR\t%s\n", r.Cluster, r.Error)
		}
		for _, s := range r.Result {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Cluster, s.Namespace, s.Name, s.Spec.Type, s.Status.Phase, trunc(s.Status.Message, 80))
		}
	}
	return w.Flush()
}

func printSims(sims []simv1.Simulation, detail bool) error {
	if out == "json" {
		if len(sims) == 1 && detail {
			return printJSON(sims[0])
		}
		return printJSON(sims)
	}
	sort.Slice(sims, func(a, b int) bool { return sims[a].CreationTimestamp.Before(&sims[b].CreationTimestamp) })
	w := tab()
	fmt.Fprintln(w, "NAMESPACE\tNAME\tTYPE\tPHASE\tINJECTED\tRECOVERED\tMESSAGE")
	for _, s := range sims {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.Namespace, s.Name, s.Spec.Type, s.Status.Phase,
			ts(s.Status.InjectedAt), ts(s.Status.RecoveredAt), trunc(s.Status.Message, 80))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if detail && len(sims) == 1 {
		for _, t := range sims[0].Status.Targets {
			fmt.Printf("  target: %s %s/%s node=%s gpu=%s %s\n", t.Kind, t.Namespace, t.Name, t.Node, t.GPU, t.Detail)
		}
		for k, v := range sims[0].Status.Data {
			fmt.Printf("  %s: %s\n", k, v)
		}
	}
	return nil
}

func ts(t *metav1.Time) string {
	if t == nil {
		return "-"
	}
	return t.Local().Format("15:04:05")
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}

func tab() *tabwriter.Writer { return tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0) }

func printJSON(v any) error {
	e := json.NewEncoder(os.Stdout)
	e.SetIndent("", "  ")
	return e.Encode(v)
}

func oneArg(args []string, use string) (string, error) {
	if len(args) != 1 {
		return "", fmt.Errorf("usage: %s", use)
	}
	return args[0], nil
}

func dptr(d time.Duration) *metav1.Duration {
	if d == 0 {
		return nil
	}
	return &metav1.Duration{Duration: d}
}

func nilIfEmpty(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return m
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

// splitName parses "name=value"; a bare value gets def. URL values keep '=' in queries.
func splitName(v, def, scheme string) (string, string) {
	if scheme != "" && strings.HasPrefix(v, scheme) {
		return def, v
	}
	if n, rest, ok := strings.Cut(v, "="); ok && !strings.ContainsAny(n, "/:") {
		return n, rest
	}
	return def, v
}

func cmdReport(ctx context.Context, c *apic.Client, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: report <name> [--wait] [--timeout 30m]")
	}
	f := flag.NewFlagSet("report", flag.ExitOnError)
	wait := f.Bool("wait", false, "wait until the observation is complete")
	timeout := f.Duration("timeout", 30*time.Minute, "max wait")
	_ = f.Parse(args[1:])
	if *wait {
		wctx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		if _, err := c.WaitObserved(wctx, defaultNS(), args[0]); err != nil {
			return err
		}
	}
	return printReport(ctx, c, defaultNS(), args[0])
}

func printReport(ctx context.Context, c *apic.Client, namespace, name string) error {
	r, err := c.Report(ctx, namespace, name)
	if err != nil {
		return err
	}
	if out == "json" {
		return printJSON(r)
	}
	live := ""
	if r.Live {
		live = " (live, observation not complete)"
	}
	fmt.Printf("%s/%s  type=%s  phase=%s%s\n", r.Namespace, r.Name, r.Type, r.Phase, live)
	for _, k := range []string{"timeToRecover", "recoveryDuration"} {
		if v := r.Data[k]; v != "" {
			fmt.Printf("  %s: %s\n", k, v)
		}
	}
	for _, w := range r.Windows {
		fmt.Printf("  window %-9s %s → %s\n", w.Name, w.Start, w.End)
	}
	if r.Observation == nil {
		return nil
	}
	if r.Observation.Message != "" {
		fmt.Println("  note:", r.Observation.Message)
	}
	if len(r.Observation.Metrics) > 0 {
		w := tab()
		fmt.Fprintln(w, "\nMETRIC\tUNIT\tBASELINE\tFAULT\tRECOVERY\tAFTER\tFAULT Δ%\tAFTER Δ%\tERROR")
		for _, m := range r.Observation.Metrics {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", m.Name, m.Unit,
				m.Windows["baseline"].Mean, m.Windows["fault"].Mean, m.Windows["recovery"].Mean, m.Windows["after"].Mean,
				m.FaultChangePercent, m.AfterChangePercent, trunc(m.Error, 60))
		}
		_ = w.Flush()
	}
	for _, p := range r.Observation.Probes {
		w := tab()
		fmt.Fprintf(w, "\nPROBE %s\tCOUNT\tFAIL\tSUCCESS\tP50ms\tP95ms\tP99ms\tMAXms\tLONGEST-OUTAGE\n", p.Name)
		for _, n := range []string{"baseline", "fault", "recovery", "after"} {
			s, ok := p.Windows[n]
			if !ok {
				continue
			}
			fmt.Fprintf(w, "  %s\t%d\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n", n, s.Count, s.Failures, s.SuccessRate, s.LatencyP50Ms, s.LatencyP95Ms, s.LatencyP99Ms, s.LatencyMaxMs, s.LongestOutage)
		}
		_ = w.Flush()
	}
	return nil
}

func cmdMetrics(ctx context.Context, c *apic.Client, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: metrics <name> [--step 30s] > out.csv")
	}
	f := flag.NewFlagSet("metrics", flag.ExitOnError)
	step := f.String("step", "", "range-query step")
	_ = f.Parse(args[1:])
	b, err := c.MetricsCSV(ctx, defaultNS(), args[0], *step)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(b)
	return err
}

func cmdProfiles(ctx context.Context, c *apic.Client) error {
	var p map[string][]simv1.MetricQuery
	if err := c.Do(ctx, "GET", c.Path("/profiles"), nil, &p); err != nil {
		return err
	}
	if out == "json" {
		return printJSON(p)
	}
	names := make([]string, 0, len(p))
	for n := range p {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Println(n)
		for _, q := range p[n] {
			fmt.Printf("  %-28s %s\n", q.Name, q.Query)
		}
	}
	return nil
}

// parseExecProbe parses <namespace>/<pod or key=value>:<command words>.
func parseExecProbe(v string) (*simv1.ExecProbe, error) {
	target, cmd, ok := strings.Cut(v, ":")
	ns, pod, ok2 := strings.Cut(target, "/")
	if !ok || !ok2 || ns == "" || pod == "" || strings.TrimSpace(cmd) == "" {
		return nil, fmt.Errorf("--probe-exec %q: want <namespace>/<pod or label=value>:<command>", v)
	}
	ep := &simv1.ExecProbe{Namespace: ns, Command: strings.Fields(cmd)}
	if k, val, isSel := strings.Cut(pod, "="); isSel {
		ep.PodSelector = map[string]string{k: val}
	} else {
		ep.Pod = pod
	}
	return ep, nil
}
