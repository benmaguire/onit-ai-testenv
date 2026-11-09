package inject

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simv1 "github.com/benmaguire/onit-ai-testenv/simulations/controller/api/v1alpha1"
	"github.com/benmaguire/onit-ai-testenv/simulations/controller/internal/kube"
	"github.com/benmaguire/onit-ai-testenv/simulations/controller/pkg/catalog"
)

// Generic fault injection for any workload. Faults run in an ephemeral
// container (Env.ToolsImage, nicolaka/netshoot by default) added to each
// target pod: it shares the pod's network namespace, so tc/iptables/ip change
// only that pod's traffic, and the pod's cgroup, so stress competes with the
// workload for its own resources.

const (
	dataDests = "destinations"
	refNetem  = "https://man7.org/linux/man-pages/man8/tc-netem.8.html"
	// DefaultToolsImage provides tc, iptables, ip, bash and python3.
	DefaultToolsImage = "nicolaka/netshoot:v0.16"
)

var (
	faultTargets = []string{"namespace", "podSelector", "graph", "component", "count"}
	durationRE   = regexp.MustCompile(`^\d+(\.\d+)?(us|ms|s)$`)
	rateRE       = regexp.MustCompile(`^\d+(\.\d+)?(bit|kbit|mbit|gbit|bps|kbps|mbps|gbps)$`)
	ifaceRE      = regexp.MustCompile(`^[a-zA-Z0-9_.@-]{1,15}$`)
	sizeRE       = regexp.MustCompile(`^\d+(Mi|Gi|M|G)?$`)
	requiresEph  = "ephemeral containers with NET_ADMIN/NET_RAW (blocked by the restricted/baseline Pod Security profiles)"
)

// faultKind selects how a podFault is applied.
type faultKind int

const (
	kindOneShot faultKind = iota // inject script runs to completion; recover script reverts
	kindLong                     // inject starts a long-running process; recover stops it (exec)
)

// podFault is a fault applied per target pod by an ephemeral container script.
type podFault struct {
	entry    catalog.Entry
	kind     faultKind
	caps     []corev1.Capability
	needDest bool
	// group: faults in the same group may not overlap on a pod (e.g. one tc root qdisc).
	group    string
	scope    podScope
	validate func(p params) error
	// script builds the inject script; dests are resolved destination CIDRs.
	script func(sim *simv1.Simulation, p params, dests []string) string
	// recover builds the recover script (oneShot) or the exec command for the
	// long-running container (long).
	recover func(sim *simv1.Simulation, p params, dests []string) string
	// targetContainer: share the process namespace of the pod's first container.
	targetContainer bool
	// verifyRestart: recovery also requires the first container's restartCount
	// to have increased (proves a container-kill took effect).
	verifyRestart bool
}

func (f *podFault) Entry() catalog.Entry { return f.entry }

func (f *podFault) Validate(sim *simv1.Simulation) error {
	if err := checkKnownParams(f.entry, sim); err != nil {
		return err
	}
	if f.scope.needsTarget() && !hasPodTarget(sim) {
		return fmt.Errorf("%s needs target.podSelector (or graph/component)", f.entry.Type)
	}
	p := newParams(f.entry, sim)
	if i := p.str("interface"); i != "" && !ifaceRE.MatchString(i) {
		return fmt.Errorf("param \"interface\": %q is not an interface name", i)
	}
	for _, port := range p.list("ports") {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("param \"ports\": %q is not a port", port)
		}
	}
	for _, d := range p.list("destinations") {
		if err := validDest(d); err != nil {
			return err
		}
	}
	if f.needDest && len(p.list("destinations")) == 0 {
		return fmt.Errorf("param \"destinations\" is required for %s", f.entry.Type)
	}
	if f.validate != nil {
		return f.validate(p)
	}
	return nil
}

func validDest(d string) error {
	if strings.HasPrefix(d, "svc/") {
		parts := strings.Split(strings.TrimPrefix(d, "svc/"), "/")
		if len(parts) < 1 || len(parts) > 2 || parts[len(parts)-1] == "" {
			return fmt.Errorf("destination %q: want svc/<name> or svc/<namespace>/<name>", d)
		}
		return nil
	}
	if net.ParseIP(d) != nil {
		return nil
	}
	if _, _, err := net.ParseCIDR(d); err == nil {
		return nil
	}
	return fmt.Errorf("destination %q: want an IP, a CIDR, svc/<name> or svc/<namespace>/<name>", d)
}

// resolveDests turns destinations into CIDRs. A Service resolves to its
// ClusterIPs and its endpoints' pod IPs (pod-to-pod and headless traffic).
func resolveDests(ctx context.Context, c client.Client, ns string, dests []string) ([]string, error) {
	var out []string
	for _, d := range dests {
		if !strings.HasPrefix(d, "svc/") {
			if ip := net.ParseIP(d); ip != nil {
				d += "/32"
			}
			out = append(out, d)
			continue
		}
		parts := strings.Split(strings.TrimPrefix(d, "svc/"), "/")
		sns, name := ns, parts[0]
		if len(parts) == 2 {
			sns, name = parts[0], parts[1]
		}
		var svc corev1.Service
		if err := c.Get(ctx, types.NamespacedName{Namespace: sns, Name: name}, &svc); err != nil {
			return nil, fmt.Errorf("destination %s: %w", d, err)
		}
		for _, ip := range svc.Spec.ClusterIPs {
			if ip != "" && ip != corev1.ClusterIPNone && net.ParseIP(ip).To4() != nil {
				out = append(out, ip+"/32")
			}
		}
		var eps discoveryv1.EndpointSliceList
		if err := c.List(ctx, &eps, client.InNamespace(sns), client.MatchingLabels{discoveryv1.LabelServiceName: name}); err != nil {
			return nil, err
		}
		for _, s := range eps.Items {
			for _, e := range s.Endpoints {
				for _, a := range e.Addresses {
					if net.ParseIP(a).To4() != nil {
						out = append(out, a+"/32")
					}
				}
			}
		}
	}
	sort.Strings(out)
	return dedupe(out), nil
}

func dedupe(s []string) []string {
	var out []string
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

func ephName(sim *simv1.Simulation, op string) string {
	uid := strings.ReplaceAll(string(sim.UID), "-", "")
	if len(uid) > 8 {
		uid = uid[:8]
	}
	return fmt.Sprintf("sim-%s-%s-%d", uid, op, time.Now().UnixNano()%1_000_000)
}

func (f *podFault) image(env *Env) string {
	if env.ToolsImage != "" {
		return env.ToolsImage
	}
	return DefaultToolsImage
}

func (f *podFault) Inject(ctx context.Context, env *Env, sim *simv1.Simulation) (Result, error) {
	if env.Ephemeral == nil {
		return Result{}, errors.New("ephemeral container runner not configured")
	}
	ns, sel := f.scope.resolve(env, sim)
	pods, err := listPods(ctx, env.Client, ns, sel)
	if err != nil {
		return Result{}, err
	}
	pods = limit(runningPods(pods), sim.Spec.Target.Count)
	if len(pods) == 0 {
		return Result{}, fmt.Errorf("no running pods match %v in %q", sel, ns)
	}
	if f.group != "" {
		if err := checkOverlap(ctx, env, sim, f.group, pods); err != nil {
			return Result{}, err
		}
	}
	p := newParams(f.entry, sim)
	dests, err := resolveDests(ctx, env.Client, ns, p.list("destinations"))
	if err != nil {
		return Result{}, err
	}
	if f.needDest && len(dests) == 0 {
		return Result{}, errors.New("destinations resolved to no addresses")
	}
	res := Result{Data: map[string]string{dataDests: strings.Join(dests, ",")}}
	if f.group != "" {
		res.Data["group"] = f.group
	}
	recordScope(&res, ns, sel, podsReady(pods))
	script := f.script(sim, p, dests)
	for i := range pods {
		pod := &pods[i]
		spec := kube.EphemeralSpec{Name: ephName(sim, "inject"), Image: f.image(env), Capabilities: f.caps,
			Command: []string{"bash", "-c", script}}
		if f.targetContainer && len(pod.Spec.Containers) > 0 {
			spec.TargetContainer = pod.Spec.Containers[0].Name
		}
		wait := 2 * time.Minute
		if f.kind == kindLong {
			wait = 0
		}
		if _, err := env.Ephemeral.Run(ctx, pod.Namespace, pod.Name, spec, wait); err != nil {
			return res, fmt.Errorf("pod %s/%s: %w", pod.Namespace, pod.Name, err)
		}
		res.Targets = append(res.Targets, simv1.AffectedTarget{Kind: "Pod", Namespace: pod.Namespace, Name: pod.Name, Node: pod.Spec.NodeName, Detail: spec.Name})
		if f.verifyRestart {
			res.Data["restarts."+pod.Name] = strconv.Itoa(int(restartCount(pod)))
		}
	}
	scopeMsg := ""
	if len(dests) > 0 {
		scopeMsg = " towards " + strings.Join(dests, ",")
	}
	res.Message = fmt.Sprintf("%s on %d pod(s)%s", f.entry.Type, len(res.Targets), scopeMsg)
	return res, nil
}

func (f *podFault) Recover(ctx context.Context, env *Env, sim *simv1.Simulation) (string, error) {
	p := newParams(f.entry, sim)
	dests := strings.Split(sim.Status.Data[dataDests], ",")
	if sim.Status.Data[dataDests] == "" {
		dests = nil
	}
	cmd := f.recover(sim, p, dests)
	var errs []error
	for _, t := range sim.Status.Targets {
		if t.Kind != "Pod" {
			continue
		}
		var err error
		if f.kind == kindLong {
			// Stop the long-running process in its own ephemeral container.
			_, _, err = env.Exec.Exec(ctx, t.Namespace, t.Name, t.Detail, []string{"bash", "-c", cmd})
			if err != nil && isGone(err) {
				err = nil // container already exited (e.g. its timeout elapsed)
			}
		} else {
			spec := kube.EphemeralSpec{Name: ephName(sim, "recover"), Image: f.image(env), Capabilities: f.caps, Command: []string{"bash", "-c", cmd}}
			_, err = env.Ephemeral.Run(ctx, t.Namespace, t.Name, spec, 2*time.Minute)
		}
		if err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("pod %s/%s: %w", t.Namespace, t.Name, err))
		}
	}
	return "reverted " + f.entry.Type, errors.Join(errs...)
}

func isGone(err error) bool {
	s := err.Error()
	for _, k := range []string{"not found", "not running", "terminated", "stopped", "exited", "is not valid"} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// Verify: the targeted pods are Ready again (the fault itself was reverted by Recover).
func (f *podFault) Verify(ctx context.Context, env *Env, sim *simv1.Simulation) (bool, string, error) {
	if f.verifyRestart {
		for _, t := range sim.Status.Targets {
			var pod corev1.Pod
			if err := env.Client.Get(ctx, types.NamespacedName{Namespace: t.Namespace, Name: t.Name}, &pod); err != nil {
				return false, "", client.IgnoreNotFound(err)
			}
			before, _ := strconv.Atoi(sim.Status.Data["restarts."+t.Name])
			if int(restartCount(&pod)) <= before {
				return false, fmt.Sprintf("container in %s has not restarted yet (restartCount %d)", t.Name, restartCount(&pod)), nil
			}
		}
	}
	return verifyScopeReady(ctx, env, sim, nil)
}

func restartCount(p *corev1.Pod) int32 {
	if len(p.Spec.Containers) == 0 {
		return 0
	}
	for _, st := range p.Status.ContainerStatuses {
		if st.Name == p.Spec.Containers[0].Name {
			return st.RestartCount
		}
	}
	return 0
}

func runningPods(pods []corev1.Pod) []corev1.Pod {
	var out []corev1.Pod
	for _, p := range pods {
		if p.Status.Phase == corev1.PodRunning {
			out = append(out, p)
		}
	}
	return out
}

// checkOverlap refuses a fault in the same group on a pod another active
// simulation already impairs (e.g. two tc root qdiscs on one interface).
func checkOverlap(ctx context.Context, env *Env, sim *simv1.Simulation, group string, pods []corev1.Pod) error {
	var list simv1.SimulationList
	if err := env.Client.List(ctx, &list); err != nil {
		return err
	}
	busy := map[string]string{}
	for _, s := range list.Items {
		if s.UID == sim.UID || (s.Status.Phase != simv1.PhaseActive && s.Status.Phase != simv1.PhaseRecovering) || s.Status.Data["group"] != group {
			continue
		}
		for _, t := range s.Status.Targets {
			if t.Kind == "Pod" {
				busy[t.Namespace+"/"+t.Name] = s.Namespace + "/" + s.Name
			}
		}
	}
	for _, p := range pods {
		if other, ok := busy[p.Namespace+"/"+p.Name]; ok {
			return fmt.Errorf("pod %s/%s already has an active %s fault from simulation %s", p.Namespace, p.Name, group, other)
		}
	}
	return nil
}

// --- netem -------------------------------------------------------------------------

func iface(p params) string {
	if i := p.str("interface"); i != "" {
		return i
	}
	return "eth0"
}

func netemArgs(p params) string {
	var a []string
	if d := p.str("delay"); d != "" && d != "0" && d != "0ms" {
		a = append(a, "delay", d)
		if j := p.str("jitter"); j != "" && j != "0" && j != "0ms" {
			a = append(a, j)
			if c := p.str("correlation"); c != "" && c != "0" {
				a = append(a, c+"%")
			}
			if dist := p.str("distribution"); dist != "" {
				a = append(a, "distribution", dist)
			}
		}
	}
	pct := func(k, kw string, corrKey string) {
		if v := p.str(k); v != "" && v != "0" {
			a = append(a, kw, v+"%")
			if corrKey != "" {
				if c := p.str(corrKey); c != "" && c != "0" {
					a = append(a, c+"%")
				}
			}
		}
	}
	pct("loss", "loss", "lossCorrelation")
	pct("corrupt", "corrupt", "")
	pct("duplicate", "duplicate", "")
	pct("reorder", "reorder", "reorderCorrelation")
	if r := p.str("rate"); r != "" {
		a = append(a, "rate", r)
	}
	return strings.Join(a, " ")
}

func netemScript(_ *simv1.Simulation, p params, dests []string) string {
	ifc := iface(p)
	ports := p.list("ports")
	var b strings.Builder
	fmt.Fprintf(&b, "set -e\nIF=%s\n", ifc)
	b.WriteString("if tc qdisc show dev $IF | grep -qE 'netem|qdisc prio 1:'; then echo \"$IF is already impaired\" >&2; exit 3; fi\n")
	if len(dests) == 0 && len(ports) == 0 {
		fmt.Fprintf(&b, "tc qdisc replace dev $IF root handle 1: netem %s\n", netemArgs(p))
	} else {
		b.WriteString("tc qdisc replace dev $IF root handle 1: prio bands 4 priomap 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0\n")
		fmt.Fprintf(&b, "tc qdisc add dev $IF parent 1:4 handle 40: netem %s\n", netemArgs(p))
		if len(dests) == 0 {
			dests = []string{"0.0.0.0/0"}
		}
		for _, d := range dests {
			if len(ports) == 0 {
				fmt.Fprintf(&b, "tc filter add dev $IF protocol ip parent 1:0 prio 1 u32 match ip dst %s flowid 1:4\n", d)
			}
			for _, port := range ports {
				fmt.Fprintf(&b, "tc filter add dev $IF protocol ip parent 1:0 prio 1 u32 match ip dst %s match ip dport %s 0xffff flowid 1:4\n", d, port)
			}
		}
	}
	b.WriteString("tc qdisc show dev $IF\n")
	return b.String()
}

func netemRecover(_ *simv1.Simulation, p params, _ []string) string {
	return fmt.Sprintf("IF=%s\ntc qdisc del dev $IF root 2>/dev/null || true\ntc qdisc show dev $IF\n", iface(p))
}

func netemParams(defaults map[string]string, keys ...string) []catalog.Param {
	desc := map[string]string{
		"delay":              "Added latency, e.g. 100ms.",
		"jitter":             "Latency variation (±), e.g. 20ms.",
		"correlation":        "Jitter correlation with the previous packet, percent.",
		"distribution":       "Jitter distribution: normal, pareto, paretonormal (empty = uniform).",
		"loss":               "Packet loss, percent (e.g. 5 or 0.5).",
		"lossCorrelation":    "Loss correlation (burstiness), percent.",
		"corrupt":            "Packets with a corrupted bit, percent.",
		"duplicate":          "Duplicated packets, percent.",
		"reorder":            "Packets sent immediately (out of order), percent; needs delay.",
		"reorderCorrelation": "Reorder correlation, percent.",
		"rate":               "Bandwidth limit, e.g. 1mbit, 512kbit.",
	}
	var out []catalog.Param
	for _, k := range keys {
		out = append(out, catalog.Param{Name: k, Description: desc[k], Default: defaults[k]})
	}
	return append(out,
		catalog.Param{Name: "destinations", Description: "Limit to traffic towards these: comma-separated IPs, CIDRs, svc/<name> or svc/<namespace>/<name>. Empty = all egress."},
		catalog.Param{Name: "ports", Description: "Limit to these destination ports (comma-separated)."},
		catalog.Param{Name: "interface", Description: "Interface in the pod.", Default: "eth0"},
	)
}

func validateNetem(p params) error {
	for _, k := range []string{"delay", "jitter"} {
		if v := p.str(k); v != "" && v != "0" && !durationRE.MatchString(v) {
			return fmt.Errorf("param %q: %q must be like 100ms", k, v)
		}
	}
	for _, k := range []string{"correlation", "loss", "lossCorrelation", "corrupt", "duplicate", "reorder", "reorderCorrelation"} {
		if p.str(k) != "" {
			if _, err := p.float(k, 0, 100); err != nil {
				return err
			}
		}
	}
	if d := p.str("distribution"); d != "" {
		if err := oneOf("distribution", d, "normal", "pareto", "paretonormal"); err != nil {
			return err
		}
		if p.str("jitter") == "" {
			return errors.New("param \"distribution\" needs jitter")
		}
	}
	if r := p.str("reorder"); r != "" && r != "0" && p.str("delay") == "" {
		return errors.New("param \"reorder\" needs delay")
	}
	if r := p.str("rate"); r != "" && !rateRE.MatchString(r) {
		return fmt.Errorf("param \"rate\": %q must be like 10mbit", r)
	}
	if netemArgs(p) == "" {
		return errors.New("no impairment configured (set delay, loss, rate, ...)")
	}
	return nil
}

func netem(typ, summary, effect string, defaults map[string]string, keys ...string) *podFault {
	return &podFault{
		entry: catalog.Entry{
			Type: typ, Category: "fault", Summary: summary, Effect: effect + " Applies to the pods' egress; to slow a service's responses, target the service's pods, to slow its callers' requests, target the callers.",
			Recovery:   "`tc qdisc del dev <if> root` restores the default queueing; recovered when the pods are Ready.",
			Targets:    faultTargets,
			Params:     netemParams(defaults, keys...),
			Requires:   []string{requiresEph},
			References: []string{refNetem},
		},
		kind: kindOneShot, caps: []corev1.Capability{"NET_ADMIN"}, group: "tc",
		validate: validateNetem, script: netemScript, recover: netemRecover,
	}
}

// --- iptables outages ------------------------------------------------------------------

func ruleTag(sim *simv1.Simulation) string {
	t := "sim-" + sim.Name
	if len(t) > 200 {
		t = t[:200]
	}
	return t
}

func outageScript(sim *simv1.Simulation, p params, dests []string) string {
	action := "REJECT"
	if p.str("mode") == "drop" {
		action = "DROP"
	}
	proto := p.str("protocol")
	ports := p.list("ports")
	if len(dests) == 0 {
		dests = []string{"0.0.0.0/0"}
	}
	var b strings.Builder
	b.WriteString("set -e\n")
	for _, d := range dests {
		protos := []string{proto}
		if proto == "all" && len(ports) > 0 {
			protos = []string{"tcp", "udp"}
		}
		for _, pr := range protos {
			args := fmt.Sprintf("-d %s", d)
			if pr != "all" {
				args += " -p " + pr
				if len(ports) > 0 {
					args += " -m multiport --dports " + strings.Join(ports, ",")
				}
			}
			target := action
			if action == "REJECT" && pr == "tcp" {
				target = "REJECT --reject-with tcp-reset"
			}
			fmt.Fprintf(&b, "iptables -I OUTPUT %s -m comment --comment %q -j %s\n", args, ruleTag(sim), target)
		}
	}
	b.WriteString("iptables -S OUTPUT\n")
	return b.String()
}

func outageRecover(sim *simv1.Simulation, _ params, _ []string) string {
	return fmt.Sprintf("set -e\niptables-save > /tmp/rules\ngrep -v -- %q /tmp/rules | iptables-restore\niptables -S OUTPUT\n", ruleTag(sim))
}

func validateOutage(p params) error {
	if err := oneOf("mode", p.str("mode"), "reject", "drop"); err != nil {
		return err
	}
	return oneOf("protocol", p.str("protocol"), "tcp", "udp", "all")
}

// --- stress -------------------------------------------------------------------------------

const stressTrap = "trap 'kill $(jobs -p) 2>/dev/null; exit 0' TERM INT\n"

func holdSeconds(sim *simv1.Simulation) int {
	if sim.Spec.Duration != nil {
		return int(sim.Spec.Duration.Duration.Seconds()) + 30
	}
	return 24 * 3600
}

func cpuStressScript(sim *simv1.Simulation, p params, _ []string) string {
	workers, _ := p.int("workers", 1, 256)
	load, _ := p.int("load", 1, 100)
	return stressTrap + fmt.Sprintf(`cat > /tmp/burn.py <<'PY'
import time, sys
load = %d / 100.0
while True:
    t = time.time()
    while time.time() - t < 0.1 * load:
        pass
    time.sleep(0.1 * (1 - load))
PY
for i in $(seq %d); do python3 /tmp/burn.py & done
sleep %d & wait $!
kill $(jobs -p) 2>/dev/null; exit 0
`, load, workers, holdSeconds(sim))
}

func memStressScript(sim *simv1.Simulation, p params, _ []string) string {
	return stressTrap + fmt.Sprintf(`python3 - <<'PY' &
import time
n = %d
b = bytearray(n)
for i in range(0, n, 4096):
    b[i] = 1
time.sleep(%d)
PY
wait $!
`, sizeBytes(p.str("size")), holdSeconds(sim))
}

func sizeBytes(s string) int64 {
	m := regexp.MustCompile(`^(\d+)(Mi|Gi|M|G)?$`).FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	switch m[2] {
	case "Mi":
		return n << 20
	case "Gi":
		return n << 30
	case "M":
		return n * 1000 * 1000
	case "G":
		return n * 1000 * 1000 * 1000
	}
	return n
}

// stopLong ends the long-running ephemeral container (bash is PID 1 with a TERM trap).
func stopLong(*simv1.Simulation, params, []string) string { return "kill -TERM 1" }

// --- link faults (network simulations, e.g. KNE routers) -------------------------------------

func linkDownScript(_ *simv1.Simulation, p params, _ []string) string {
	return fmt.Sprintf("set -e\nip link set dev %s down\nip -br link show dev %s\n", iface(p), iface(p))
}

func linkUpScript(_ *simv1.Simulation, p params, _ []string) string {
	return fmt.Sprintf("set -e\nip link set dev %s up\nip -br link show dev %s\n", iface(p), iface(p))
}

func linkFlapScript(sim *simv1.Simulation, p params, _ []string) string {
	down, _ := p.int("downSeconds", 1, 3600)
	up, _ := p.int("upSeconds", 1, 3600)
	return fmt.Sprintf(`trap 'ip link set dev %[1]s up; exit 0' TERM INT
end=$(( $(date +%%s) + %[4]d ))
while [ $(date +%%s) -lt $end ]; do
  ip link set dev %[1]s down; sleep %[2]d & wait $!
  ip link set dev %[1]s up;   sleep %[3]d & wait $!
done
ip link set dev %[1]s up
`, iface(p), down, up, holdSeconds(sim))
}

func linkFlapRecover(*simv1.Simulation, params, []string) string { return "kill -TERM 1" }

// FaultInjectors returns the generic network, outage, resource and link faults.
func FaultInjectors() []Injector {
	none := map[string]string{}
	outageParams := []catalog.Param{
		{Name: "destinations", Description: "Comma-separated IPs, CIDRs, svc/<name> or svc/<namespace>/<name> the pods can no longer reach.", Required: true},
		{Name: "ports", Description: "Only these destination ports (comma-separated)."},
		{Name: "protocol", Description: "tcp, udp or all.", Default: "tcp"},
		{Name: "mode", Description: "reject (fast failure: TCP reset / ICMP unreachable) or drop (silent: callers time out).", Default: "reject"},
	}
	return []Injector{
		netem("net-latency", "Add network latency to pods' traffic",
			"Every egress packet (or only those towards destinations/ports) is delayed.",
			map[string]string{"delay": "100ms"}, "delay", "jitter", "correlation", "distribution"),
		netem("net-jitter", "Add variable latency (jitter)",
			"Packets get a random delay of delay ± jitter with the chosen distribution, so latency varies and packets may reorder.",
			map[string]string{"delay": "50ms", "jitter": "40ms", "distribution": "normal", "correlation": "25"}, "delay", "jitter", "correlation", "distribution"),
		netem("net-packet-loss", "Drop a percentage of packets",
			"Random (or bursty, with lossCorrelation) packet loss; TCP retransmits and slows, UDP loses data.",
			map[string]string{"loss": "10"}, "loss", "lossCorrelation"),
		netem("net-corruption", "Corrupt a percentage of packets",
			"Single-bit errors in packets; receivers drop them on checksum failure (looks like loss with retransmits).",
			map[string]string{"corrupt": "5"}, "corrupt"),
		netem("net-duplication", "Duplicate a percentage of packets",
			"Packets are sent twice; tests idempotency and duplicate handling of UDP protocols.",
			map[string]string{"duplicate": "5"}, "duplicate"),
		netem("net-reorder", "Reorder packets",
			"A percentage of packets skip the delay and arrive before earlier ones.",
			map[string]string{"delay": "50ms", "reorder": "25", "reorderCorrelation": "50"}, "delay", "reorder", "reorderCorrelation"),
		netem("net-bandwidth", "Limit bandwidth",
			"Egress is limited to the given rate; large responses (e.g. streamed tokens, model downloads) slow down and queues build.",
			map[string]string{"rate": "1mbit"}, "rate", "delay"),
		netem("net-degraded", "Combined impairment: latency, jitter, loss and bandwidth",
			"Any combination of netem impairments in one fault, e.g. a congested WAN link.",
			none, "delay", "jitter", "correlation", "distribution", "loss", "lossCorrelation", "corrupt", "duplicate", "reorder", "rate"),

		&podFault{
			entry: catalog.Entry{
				Type: "dependency-outage", Category: "fault", Summary: "Target pods can no longer reach a dependency",
				Effect:   "iptables rules in the pods reject (connection refused/reset) or drop (timeouts) traffic towards the destinations — a database, cache, model store or downstream service outage as the caller sees it.",
				Recovery: "The simulation's tagged iptables rules are removed; recovered when the pods are Ready.",
				Targets:  faultTargets, Params: outageParams, Requires: []string{requiresEph},
			},
			kind: kindOneShot, caps: []corev1.Capability{"NET_ADMIN", "NET_RAW"}, needDest: true,
			validate: validateOutage, script: outageScript, recover: outageRecover,
		},
		&podFault{
			entry: catalog.Entry{
				Type: "dns-outage", Category: "fault", Summary: "DNS resolution fails for target pods",
				Effect:   "Traffic to port 53 (UDP and TCP) from the pods is dropped or rejected, so name lookups time out or fail while existing connections keep working.",
				Recovery: "The tagged iptables rules are removed; recovered when the pods are Ready.",
				Targets:  faultTargets,
				Params:   []catalog.Param{{Name: "mode", Description: "drop (lookups time out) or reject (fail fast).", Default: "drop"}, {Name: "protocol", Description: "Fixed to all (UDP and TCP).", Default: "all"}, {Name: "ports", Description: "Fixed to 53.", Default: "53"}},
				Requires: []string{requiresEph},
			},
			kind: kindOneShot, caps: []corev1.Capability{"NET_ADMIN", "NET_RAW"},
			validate: validateOutage, script: outageScript, recover: outageRecover,
		},
		&serviceOutage{},
		&podFault{
			entry: catalog.Entry{
				Type: "cpu-stress", Category: "fault", Summary: "Burn CPU inside target pods",
				Effect:   "Busy workers in an ephemeral container share the pod's cgroup, competing with the workload for its CPU limit (throttling, higher latency) — a noisy neighbour or runaway thread.",
				Recovery: "The stress process is stopped (or ends after duration + 30s); recovered when the pods are Ready.",
				Targets:  faultTargets,
				Params:   []catalog.Param{{Name: "workers", Description: "Busy processes.", Default: "1"}, {Name: "load", Description: "Load per worker, percent.", Default: "100"}},
				Requires: []string{"ephemeral containers"},
			},
			kind: kindLong,
			validate: func(p params) error {
				if _, err := p.int("workers", 1, 256); err != nil {
					return err
				}
				_, err := p.int("load", 1, 100)
				return err
			},
			script: cpuStressScript, recover: stopLong,
		},
		&podFault{
			entry: catalog.Entry{
				Type: "memory-stress", Category: "fault", Summary: "Consume memory inside target pods",
				Effect:   "Allocates and touches memory in the pod's cgroup: page-cache pressure, then OOM kills if the pod's limit is exceeded (which process is killed is up to the kernel).",
				Recovery: "The allocation is released; recovered when the pods are Ready (after any OOM restarts).",
				Targets:  faultTargets,
				Params:   []catalog.Param{{Name: "size", Description: "Memory to allocate: bytes or with Mi/Gi/M/G.", Default: "256Mi"}},
				Requires: []string{"ephemeral containers"},
			},
			kind: kindLong,
			validate: func(p params) error {
				if !sizeRE.MatchString(p.str("size")) || sizeBytes(p.str("size")) <= 0 {
					return fmt.Errorf("param \"size\": %q must be like 512Mi", p.str("size"))
				}
				return nil
			},
			script: memStressScript, recover: stopLong,
		},
		&podFault{
			entry: catalog.Entry{
				Type: "container-kill", Category: "fault", Summary: "Crash the main container (restart in place)",
				Effect:   "Sends a signal to PID 1 of the pod's first container, which then restarts under its restartPolicy (restartCount increases); the pod, its IP and volumes stay. Unlike pod-kill this also works for bare pods such as KNE routers.",
				Recovery: "Nothing to revert; recovered when the pods are Ready again.",
				Targets:  faultTargets,
				Params:   []catalog.Param{{Name: "signal", Description: "KILL or TERM.", Default: "KILL"}},
				Requires: []string{"ephemeral containers with process-namespace targeting (containerd 1.6+)"},
			},
			kind: kindOneShot, targetContainer: true, verifyRestart: true,
			validate: func(p params) error { return oneOf("signal", p.str("signal"), "KILL", "TERM") },
			script: func(_ *simv1.Simulation, p params, _ []string) string {
				return "kill -" + p.str("signal") + " 1"
			},
			recover: func(*simv1.Simulation, params, []string) string { return "true" },
		},

		&podFault{
			entry: catalog.Entry{
				Type: "link-down", Category: "network", Summary: "Take a pod interface (e.g. a router link) down",
				Effect:   "ip link set <interface> down in the pod: for KNE routers the link to the neighbour goes down, so routing protocols (BGP/IS-IS/OSPF) drop the adjacency and reconverge.",
				Recovery: "ip link set <interface> up; recovered when the pods are Ready (check protocol state with probes).",
				Targets:  faultTargets,
				Params:   []catalog.Param{{Name: "interface", Description: "Interface to take down, e.g. eth1 (KNE link).", Required: true}},
				Requires: []string{requiresEph},
			},
			kind: kindOneShot, caps: []corev1.Capability{"NET_ADMIN"}, group: "link",
			script: linkDownScript, recover: linkUpScript,
		},
		&podFault{
			entry: catalog.Entry{
				Type: "link-flap", Category: "network", Summary: "Flap a pod interface down/up repeatedly",
				Effect:   "The interface goes down for downSeconds and up for upSeconds in a loop — route flap dampening, BFD/hold timers and convergence under churn.",
				Recovery: "The loop stops and the interface is left up.",
				Targets:  faultTargets,
				Params: []catalog.Param{
					{Name: "interface", Description: "Interface to flap, e.g. eth1.", Required: true},
					{Name: "downSeconds", Description: "Seconds down per cycle.", Default: "5"},
					{Name: "upSeconds", Description: "Seconds up per cycle.", Default: "20"},
				},
				Requires: []string{requiresEph},
			},
			kind: kindLong, caps: []corev1.Capability{"NET_ADMIN"}, group: "link",
			validate: func(p params) error {
				if _, err := p.int("downSeconds", 1, 3600); err != nil {
					return err
				}
				_, err := p.int("upSeconds", 1, 3600)
				return err
			},
			script: linkFlapScript, recover: linkFlapRecover,
		},
	}
}

// --- service outage ------------------------------------------------------------------------------

// serviceOutage takes a Service's backends off the network for its clients
// (deny-all ingress NetworkPolicy on the Service's selector).
type serviceOutage struct{ netIsolate }

func (s *serviceOutage) Entry() catalog.Entry {
	return catalog.Entry{
		Type: "service-outage", Category: "fault", Summary: "A Service stops answering",
		Effect:   "A deny-all ingress NetworkPolicy on the Service's backend pods: every client sees the service as down (connections time out), while the pods themselves keep running and stay Ready.",
		Recovery: "The NetworkPolicy is deleted; recovered when it is gone and the backends are Ready.",
		Targets:  []string{"namespace"},
		Params: []catalog.Param{
			{Name: "service", Description: "Service name (in target.namespace).", Required: true},
		},
		Requires: []string{"a CNI that enforces NetworkPolicy (k3s does by default)"},
	}
}

func (s *serviceOutage) Validate(sim *simv1.Simulation) error {
	return checkKnownParams(s.Entry(), sim)
}

func (s *serviceOutage) Inject(ctx context.Context, env *Env, sim *simv1.Simulation) (Result, error) {
	ns := sim.Spec.Target.Namespace
	if ns == "" {
		ns = sim.Namespace
	}
	var svc corev1.Service
	if err := env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: sim.Spec.Params["service"]}, &svc); err != nil {
		return Result{}, err
	}
	if len(svc.Spec.Selector) == 0 {
		return Result{}, fmt.Errorf("service %s/%s has no selector", ns, svc.Name)
	}
	cp := sim.DeepCopy()
	cp.Spec.Target = simv1.Target{Namespace: ns, PodSelector: svc.Spec.Selector}
	cp.Spec.Params = map[string]string{"direction": "ingress", "allowDNS": "false"}
	s.netIsolate.entry = catalog.Entry{Type: "service-outage", Params: isolateParams()}
	res, err := s.netIsolate.Inject(ctx, env, cp)
	res.Message = fmt.Sprintf("service %s/%s is down for its clients (%s)", ns, svc.Name, res.Message)
	return res, err
}
