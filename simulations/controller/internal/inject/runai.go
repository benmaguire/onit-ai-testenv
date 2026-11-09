package inject

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simv1 "github.com/benmaguire/onit-ai-testenv/simulations/controller/api/v1alpha1"
	"github.com/benmaguire/onit-ai-testenv/simulations/controller/pkg/catalog"
)

// Run:ai simulation (runai-sim environment): KWOK virtual nodes with
// fake-gpu-operator GPUs, scheduled by KAI Scheduler.
const (
	annotationKwokNode    = "kwok.x-k8s.io/node"
	kwokManagedValue      = "fake"
	kwokPausedValue       = "sim-paused"
	annotationUtilization = "run.ai/simulated-gpu-utilization"

	refKWOK = "https://kwok.sigs.k8s.io/docs/user/kwok-manage-nodes-and-pods/"
	refFGO  = "https://github.com/run-ai/fake-gpu-operator"
	refKAI  = "https://github.com/kai-scheduler/KAI-Scheduler"
)

var utilRE = regexp.MustCompile(`^\d{1,3}(-\d{1,3})?$`)

// --- kwok-node-down --------------------------------------------------------------

type kwokNodeDown struct{ entry catalog.Entry }

func (k *kwokNodeDown) Entry() catalog.Entry { return k.entry }

func (k *kwokNodeDown) Validate(sim *simv1.Simulation) error { return checkKnownParams(k.entry, sim) }

// kwokNodes resolves target nodes, defaulting to every KWOK node, and refuses
// anything that is not a KWOK-managed virtual node.
func kwokNodes(ctx context.Context, env *Env, sim *simv1.Simulation) ([]corev1.Node, error) {
	t := sim.Spec.Target
	var nodes []corev1.Node
	if t.Node != "" {
		var n corev1.Node
		if err := env.Client.Get(ctx, types.NamespacedName{Name: t.Node}, &n); err != nil {
			return nil, err
		}
		nodes = []corev1.Node{n}
	} else {
		sel := t.NodeSelector
		if len(sel) == 0 {
			sel = map[string]string{"type": "kwok"}
		}
		var nl corev1.NodeList
		if err := env.Client.List(ctx, &nl, client.MatchingLabels(sel)); err != nil {
			return nil, err
		}
		nodes = nl.Items
	}
	sort.Slice(nodes, func(a, b int) bool { return nodes[a].Name < nodes[b].Name })
	var out []corev1.Node
	for _, n := range nodes {
		switch n.Annotations[annotationKwokNode] {
		case kwokManagedValue:
			out = append(out, n)
		case kwokPausedValue:
			// already down (another simulation): leave it
		default:
			if t.Node != "" {
				return nil, fmt.Errorf("node %s is not a KWOK virtual node (annotation %s=%s missing); refusing", n.Name, annotationKwokNode, kwokManagedValue)
			}
		}
	}
	out = limit(out, t.Count)
	if len(out) == 0 {
		return nil, fmt.Errorf("no KWOK virtual nodes match the target")
	}
	return out, nil
}

func setKwokAnnotation(ctx context.Context, c client.Client, name, value string) error {
	var n corev1.Node
	if err := c.Get(ctx, types.NamespacedName{Name: name}, &n); err != nil {
		return err
	}
	patch := client.MergeFrom(n.DeepCopy())
	if n.Annotations == nil {
		n.Annotations = map[string]string{}
	}
	n.Annotations[annotationKwokNode] = value
	return c.Patch(ctx, &n, patch)
}

func (k *kwokNodeDown) Inject(ctx context.Context, env *Env, sim *simv1.Simulation) (Result, error) {
	nodes, err := kwokNodes(ctx, env, sim)
	if err != nil {
		return Result{}, err
	}
	var res Result
	for _, n := range nodes {
		if err := setKwokAnnotation(ctx, env.Client, n.Name, kwokPausedValue); err != nil {
			return res, err
		}
		res.Targets = append(res.Targets, simv1.AffectedTarget{Kind: "Node", Name: n.Name, Detail: n.Labels["run.ai/simulated-gpu-node-pool"]})
	}
	res.Message = fmt.Sprintf("stopped KWOK heartbeat on %d virtual node(s); NotReady after the node-monitor grace period, pods evicted after their not-ready toleration (default 5m)", len(res.Targets))
	return res, nil
}

func (k *kwokNodeDown) Recover(ctx context.Context, env *Env, sim *simv1.Simulation) (string, error) {
	var errs []error
	for _, t := range sim.Status.Targets {
		if t.Kind == "Node" {
			if err := setKwokAnnotation(ctx, env.Client, t.Name, kwokManagedValue); err != nil && !apierrors.IsNotFound(err) {
				errs = append(errs, err)
			}
		}
	}
	return "KWOK heartbeat resumed", errors.Join(errs...)
}

func (k *kwokNodeDown) Verify(ctx context.Context, env *Env, sim *simv1.Simulation) (bool, string, error) {
	for _, t := range sim.Status.Targets {
		var n corev1.Node
		if err := env.Client.Get(ctx, types.NamespacedName{Name: t.Name}, &n); err != nil {
			return false, "", client.IgnoreNotFound(err)
		}
		if !nodeReady(&n) {
			return false, "node " + t.Name + " not Ready yet", nil
		}
	}
	return true, "virtual nodes Ready", nil
}

func nodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// --- fgo-gpu-utilization -----------------------------------------------------------

type fgoUtilization struct{ entry catalog.Entry }

func (f *fgoUtilization) Entry() catalog.Entry { return f.entry }

func (f *fgoUtilization) Validate(sim *simv1.Simulation) error {
	if err := checkKnownParams(f.entry, sim); err != nil {
		return err
	}
	if !hasPodTarget(sim) {
		return fmt.Errorf("fgo-gpu-utilization needs target.podSelector")
	}
	r := newParams(f.entry, sim).str("range")
	if !utilRE.MatchString(r) {
		return fmt.Errorf("param \"range\": %q must be N or N-M (percent)", r)
	}
	return nil
}

func dataKey(p *corev1.Pod) string { return "orig." + p.Namespace + "." + p.Name }

const absent = "\x00absent"

func setPodAnnotation(ctx context.Context, c client.Client, ns, name, value string) error {
	var p corev1.Pod
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &p); err != nil {
		return err
	}
	patch := client.MergeFrom(p.DeepCopy())
	if value == absent {
		delete(p.Annotations, annotationUtilization)
	} else {
		if p.Annotations == nil {
			p.Annotations = map[string]string{}
		}
		p.Annotations[annotationUtilization] = value
	}
	return c.Patch(ctx, &p, patch)
}

func (f *fgoUtilization) Inject(ctx context.Context, env *Env, sim *simv1.Simulation) (Result, error) {
	ns, sel := podSelectorFor(env, sim, "")
	pods, err := listPods(ctx, env.Client, ns, sel)
	if err != nil {
		return Result{}, err
	}
	pods = limit(pods, sim.Spec.Target.Count)
	if len(pods) == 0 {
		return Result{}, fmt.Errorf("no pods match %v in %q", sel, ns)
	}
	val := newParams(f.entry, sim).str("range")
	res := Result{Data: map[string]string{}}
	for i := range pods {
		p := &pods[i]
		orig, ok := p.Annotations[annotationUtilization]
		if !ok {
			orig = absent
		}
		res.Data[dataKey(p)] = strconv.Quote(orig)
		if err := setPodAnnotation(ctx, env.Client, p.Namespace, p.Name, val); err != nil {
			return res, err
		}
		res.Targets = append(res.Targets, simv1.AffectedTarget{Kind: "Pod", Namespace: p.Namespace, Name: p.Name, Node: p.Spec.NodeName, Detail: val})
	}
	res.Message = fmt.Sprintf("GPU utilization %s%% on %d pod(s)", val, len(res.Targets))
	return res, nil
}

func (f *fgoUtilization) Recover(ctx context.Context, env *Env, sim *simv1.Simulation) (string, error) {
	var errs []error
	for _, t := range sim.Status.Targets {
		if t.Kind != "Pod" {
			continue
		}
		orig := absent
		if q, ok := sim.Status.Data["orig."+t.Namespace+"."+t.Name]; ok {
			if s, err := strconv.Unquote(q); err == nil {
				orig = s
			}
		}
		if err := setPodAnnotation(ctx, env.Client, t.Namespace, t.Name, orig); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	return "restored utilization annotations", errors.Join(errs...)
}

func (f *fgoUtilization) Verify(context.Context, *Env, *simv1.Simulation) (bool, string, error) {
	return true, "annotations restored", nil
}

// RunaiInjectors returns the runai-sim simulations. pod-kill, network-isolate
// and node-cordon from KubernetesInjectors also apply there.
func RunaiInjectors() []Injector {
	return []Injector{
		&kwokNodeDown{entry: catalog.Entry{
			Type: "kwok-node-down", Category: "runai", Summary: "A virtual GPU node fails (stops heartbeating)",
			Effect:     "The KWOK heartbeat stops, so the node goes NotReady after the node-monitor grace period (~40s) and gets the unreachable taint; its pods are evicted after their not-ready toleration (default 300s) and KAI Scheduler must place them elsewhere. GPUs on the node drop out of the queues' available capacity.",
			Recovery:   "The KWOK heartbeat resumes; recovered when the nodes are Ready again (re-scheduling of evicted workloads is up to their controllers and KAI).",
			Targets:    []string{"node", "nodeSelector", "count"},
			Requires:   []string{"runai-sim environment (KWOK virtual nodes); real nodes are refused"},
			References: []string{refKWOK, refKAI},
		}},
		&fgoUtilization{entry: catalog.Entry{
			Type: "fgo-gpu-utilization", Category: "runai", Summary: "Change the GPU utilization fake-gpu-operator reports for pods",
			Effect:     "Sets run.ai/simulated-gpu-utilization on the matched pods, so FGO's synthetic DCGM metrics report that utilization range — e.g. idle GPUs for idle-reclaim / consolidation tests or saturated ones for alerting and autoscaling.",
			Recovery:   "Each pod's previous annotation value is restored (or removed). Pods recreated by their controller get the template's value anyway.",
			Targets:    podTargets,
			Params:     []catalog.Param{{Name: "range", Description: "Percent N or range N-M, e.g. 0 or 95-100.", Default: "95-100"}},
			Requires:   []string{"runai-sim environment (fake-gpu-operator)"},
			References: []string{refFGO},
		}},
	}
}
