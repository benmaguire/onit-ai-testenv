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

	simv1 "github.com/benmaguire/onit-ai-testenv/simulations/nvidia-sim/api/v1alpha1"
	"github.com/benmaguire/onit-ai-testenv/simulations/nvidia-sim/pkg/catalog"
	apic "github.com/benmaguire/onit-ai-testenv/simulations/nvidia-sim/pkg/client"
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
	wait := f.String("wait", "", "wait for a phase: active or recovered")
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
	s, err := c.Create(ctx, apic.CreateRequest{Name: *name, Namespace: ns, Labels: nilIfEmpty(labels), Spec: spec})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "created %s/%s\n", s.Namespace, s.Name)
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
