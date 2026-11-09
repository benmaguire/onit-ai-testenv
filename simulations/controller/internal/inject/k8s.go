package inject

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simv1 "github.com/benmaguire/onit-ai-testenv/simulations/controller/api/v1alpha1"
	"github.com/benmaguire/onit-ai-testenv/simulations/controller/pkg/catalog"
)

const (
	// LabelSimulation marks objects a simulation created.
	LabelSimulation = "sim.testenv.onit.ai/simulation"

	dataNamespace = "namespace"
	dataSelector  = "selector"
	dataExpected  = "expectedReady"

	refDynamoFT = "https://docs.nvidia.com/dynamo/latest/kubernetes/fault-tolerance/overview"
)

var podTargets = []string{"namespace", "podSelector", "graph", "component", "count"}

// podScope is how a pod-based simulation finds its pods by default.
type podScope struct {
	component string            // Dynamo component (uses env default graph/namespace)
	namespace string            // fixed default namespace
	selector  map[string]string // fixed default selector
}

func (s podScope) resolve(env *Env, sim *simv1.Simulation) (string, map[string]string) {
	if s.component != "" {
		return podSelectorFor(env, sim, s.component)
	}
	ns, sel := podSelectorFor(env, sim, "")
	if len(sel) == 0 && s.selector != nil {
		for k, v := range s.selector {
			sel[k] = v
		}
	}
	if sim.Spec.Target.Namespace == "" && s.namespace != "" {
		ns = s.namespace
	}
	return ns, sel
}

func (s podScope) needsTarget() bool { return s.component == "" && s.selector == nil }

// recordScope stores where the pods are so Verify can find them again.
func recordScope(res *Result, ns string, sel map[string]string, expected int) {
	if res.Data == nil {
		res.Data = map[string]string{}
	}
	res.Data[dataNamespace] = ns
	res.Data[dataSelector] = labels.Set(sel).String()
	res.Data[dataExpected] = strconv.Itoa(expected)
}

// verifyScopeReady: at least the recorded number of pods are Ready again and
// none of the given (killed) pod UIDs remain.
func verifyScopeReady(ctx context.Context, env *Env, sim *simv1.Simulation, goneUIDs map[string]bool) (bool, string, error) {
	sel, err := labels.ConvertSelectorToLabelsMap(sim.Status.Data[dataSelector])
	if err != nil {
		return false, "", err
	}
	ns := sim.Status.Data[dataNamespace]
	expected, _ := strconv.Atoi(sim.Status.Data[dataExpected])
	pods, err := listPods(ctx, env.Client, ns, sel)
	if err != nil {
		return false, "", err
	}
	for _, p := range pods {
		if goneUIDs[string(p.UID)] {
			return false, fmt.Sprintf("pod %s still exists", p.Name), nil
		}
	}
	ready := podsReady(pods)
	if ready < expected {
		return false, fmt.Sprintf("%d/%d pods Ready", ready, expected), nil
	}
	return true, fmt.Sprintf("%d/%d pods Ready", ready, expected), nil
}

// --- pod kill ------------------------------------------------------------------

type podKill struct {
	entry catalog.Entry
	scope podScope
}

func (k *podKill) Entry() catalog.Entry { return k.entry }

// OneShot: a kill has nothing to hold.
func (k *podKill) OneShot() bool { return true }

func (k *podKill) Validate(sim *simv1.Simulation) error {
	if err := checkKnownParams(k.entry, sim); err != nil {
		return err
	}
	if k.scope.needsTarget() && !hasPodTarget(sim) {
		return fmt.Errorf("%s needs target.podSelector (or graph/component)", k.entry.Type)
	}
	_, err := newParams(k.entry, sim).int("gracePeriodSeconds", 0, 3600)
	return err
}

func (k *podKill) Inject(ctx context.Context, env *Env, sim *simv1.Simulation) (Result, error) {
	ns, sel := k.scope.resolve(env, sim)
	pods, err := listPods(ctx, env.Client, ns, sel)
	if err != nil {
		return Result{}, err
	}
	if len(pods) == 0 {
		return Result{}, fmt.Errorf("no pods match %v in %q", sel, ns)
	}
	var res Result
	// Expect the same number of Ready pods afterwards as there are now.
	recordScope(&res, ns, sel, podsReady(pods))
	grace, _ := newParams(k.entry, sim).int("gracePeriodSeconds", 0, 3600)
	g := int64(grace)
	for _, p := range limit(pods, sim.Spec.Target.Count) {
		if err := env.Client.Delete(ctx, &p, &client.DeleteOptions{GracePeriodSeconds: &g}); err != nil && !apierrors.IsNotFound(err) {
			return res, fmt.Errorf("delete pod %s/%s: %w", p.Namespace, p.Name, err)
		}
		res.Targets = append(res.Targets, simv1.AffectedTarget{Kind: "Pod", Namespace: p.Namespace, Name: p.Name, Node: p.Spec.NodeName, Detail: string(p.UID)})
	}
	res.Message = fmt.Sprintf("killed %d pod(s) matching %s in %s", len(res.Targets), labels.Set(sel), ns)
	return res, nil
}

func (k *podKill) Recover(context.Context, *Env, *simv1.Simulation) (string, error) {
	return "waiting for controllers to replace the killed pods", nil
}

func (k *podKill) Verify(ctx context.Context, env *Env, sim *simv1.Simulation) (bool, string, error) {
	gone := map[string]bool{}
	for _, t := range sim.Status.Targets {
		if t.Kind == "Pod" {
			gone[t.Detail] = true
		}
	}
	return verifyScopeReady(ctx, env, sim, gone)
}

func killParams() []catalog.Param {
	return []catalog.Param{{Name: "gracePeriodSeconds", Description: "Grace period for the delete; 0 is an abrupt kill (SIGKILL).", Default: "0"}}
}

// --- network isolation ---------------------------------------------------------

type netIsolate struct {
	entry catalog.Entry
	scope podScope
}

func (n *netIsolate) Entry() catalog.Entry { return n.entry }

func (n *netIsolate) Validate(sim *simv1.Simulation) error {
	if err := checkKnownParams(n.entry, sim); err != nil {
		return err
	}
	if n.scope.needsTarget() && !hasPodTarget(sim) {
		return fmt.Errorf("%s needs target.podSelector (or graph/component)", n.entry.Type)
	}
	p := newParams(n.entry, sim)
	if err := oneOf("direction", p.str("direction"), "ingress", "egress", "both"); err != nil {
		return err
	}
	_, err := p.bool("allowDNS")
	return err
}

func policyName(sim *simv1.Simulation) string {
	name := "sim-" + sim.Name
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.TrimRight(name, "-.")
}

func (n *netIsolate) Inject(ctx context.Context, env *Env, sim *simv1.Simulation) (Result, error) {
	ns, sel := n.scope.resolve(env, sim)
	pods, err := listPods(ctx, env.Client, ns, sel)
	if err != nil {
		return Result{}, err
	}
	var res Result
	recordScope(&res, ns, sel, podsReady(pods))

	p := newParams(n.entry, sim)
	dir := p.str("direction")
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: policyName(sim), Namespace: ns,
			Labels: map[string]string{LabelSimulation: sim.Name},
		},
		Spec: networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: sel}},
	}
	if dir == "ingress" || dir == "both" {
		np.Spec.PolicyTypes = append(np.Spec.PolicyTypes, networkingv1.PolicyTypeIngress)
	}
	if dir == "egress" || dir == "both" {
		np.Spec.PolicyTypes = append(np.Spec.PolicyTypes, networkingv1.PolicyTypeEgress)
		if allow, _ := p.bool("allowDNS"); allow {
			udp, tcp := corev1.ProtocolUDP, corev1.ProtocolTCP
			port := intstr.FromInt32(53)
			np.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: &port}, {Protocol: &tcp, Port: &port}},
			}}
		}
	}
	if ns == sim.Namespace {
		np.OwnerReferences = []metav1.OwnerReference{ownerRef(sim)}
	}
	if err := env.Client.Create(ctx, np); err != nil && !apierrors.IsAlreadyExists(err) {
		return res, fmt.Errorf("create NetworkPolicy: %w", err)
	}
	res.Targets = []simv1.AffectedTarget{{Kind: "NetworkPolicy", Namespace: ns, Name: np.Name, Detail: dir}}
	for _, pod := range pods {
		res.Targets = append(res.Targets, simv1.AffectedTarget{Kind: "Pod", Namespace: pod.Namespace, Name: pod.Name, Node: pod.Spec.NodeName})
	}
	res.Message = fmt.Sprintf("isolated %d pod(s) matching %s in %s (%s)", len(pods), labels.Set(sel), ns, dir)
	return res, nil
}

func (n *netIsolate) Recover(ctx context.Context, env *Env, sim *simv1.Simulation) (string, error) {
	var errs []error
	for _, t := range sim.Status.Targets {
		if t.Kind != "NetworkPolicy" {
			continue
		}
		np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: t.Name, Namespace: t.Namespace}}
		if err := env.Client.Delete(ctx, np); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	return "removed NetworkPolicy", errors.Join(errs...)
}

func (n *netIsolate) Verify(ctx context.Context, env *Env, sim *simv1.Simulation) (bool, string, error) {
	for _, t := range sim.Status.Targets {
		if t.Kind != "NetworkPolicy" {
			continue
		}
		err := env.Client.Get(ctx, types.NamespacedName{Namespace: t.Namespace, Name: t.Name}, &networkingv1.NetworkPolicy{})
		if err == nil {
			return false, "NetworkPolicy still present", nil
		}
		if !apierrors.IsNotFound(err) {
			return false, "", err
		}
	}
	return verifyScopeReady(ctx, env, sim, nil)
}

func isolateParams() []catalog.Param {
	return []catalog.Param{
		{Name: "direction", Description: "ingress, egress or both.", Default: "both"},
		{Name: "allowDNS", Description: "Keep DNS (port 53) egress open so only service traffic is cut.", Default: "true"},
	}
}

// --- node cordon ---------------------------------------------------------------

type nodeCordon struct{ entry catalog.Entry }

func (c *nodeCordon) Entry() catalog.Entry { return c.entry }

func (c *nodeCordon) Validate(sim *simv1.Simulation) error { return checkKnownParams(c.entry, sim) }

func (c *nodeCordon) Inject(ctx context.Context, env *Env, sim *simv1.Simulation) (Result, error) {
	nodes, err := resolveNodes(ctx, env, sim)
	if err != nil {
		return Result{}, err
	}
	var res Result
	for _, name := range nodes {
		var node corev1.Node
		if err := env.Client.Get(ctx, types.NamespacedName{Name: name}, &node); err != nil {
			return res, err
		}
		if node.Spec.Unschedulable {
			continue // already cordoned by someone else: leave it to them
		}
		patch := client.MergeFrom(node.DeepCopy())
		node.Spec.Unschedulable = true
		if err := env.Client.Patch(ctx, &node, patch); err != nil {
			return res, err
		}
		res.Targets = append(res.Targets, simv1.AffectedTarget{Kind: "Node", Name: name})
	}
	res.Message = fmt.Sprintf("cordoned %d node(s)", len(res.Targets))
	return res, nil
}

func (c *nodeCordon) Recover(ctx context.Context, env *Env, sim *simv1.Simulation) (string, error) {
	var errs []error
	for _, t := range sim.Status.Targets {
		if t.Kind != "Node" {
			continue
		}
		var node corev1.Node
		if err := env.Client.Get(ctx, types.NamespacedName{Name: t.Name}, &node); err != nil {
			if !apierrors.IsNotFound(err) {
				errs = append(errs, err)
			}
			continue
		}
		patch := client.MergeFrom(node.DeepCopy())
		node.Spec.Unschedulable = false
		errs = append(errs, env.Client.Patch(ctx, &node, patch))
	}
	return "uncordoned", errors.Join(errs...)
}

func (c *nodeCordon) Verify(ctx context.Context, env *Env, sim *simv1.Simulation) (bool, string, error) {
	for _, t := range sim.Status.Targets {
		var node corev1.Node
		if err := env.Client.Get(ctx, types.NamespacedName{Name: t.Name}, &node); err != nil {
			return false, "", client.IgnoreNotFound(err)
		}
		if node.Spec.Unschedulable {
			return false, "node " + t.Name + " still cordoned", nil
		}
	}
	return true, "nodes schedulable", nil
}

func ownerRef(sim *simv1.Simulation) metav1.OwnerReference {
	t := true
	return metav1.OwnerReference{
		APIVersion: simv1.GroupVersion.String(), Kind: "Simulation",
		Name: sim.Name, UID: sim.UID, Controller: &t,
	}
}

// KubernetesInjectors returns the pod, network and node simulations, including
// the Dynamo-aware presets for the nvidia-sim graph.
func KubernetesInjectors() []Injector {
	return []Injector{
		&podKill{scope: podScope{}, entry: catalog.Entry{
			Type: "pod-kill", Category: "kubernetes", Summary: "Kill pods matching a selector",
			Effect:   "Deletes the matched pods (abruptly by default). Their controller replaces them.",
			Recovery: "Nothing to revert; recovered when as many matching pods are Ready as before the kill.",
			Targets:  podTargets, Params: killParams(),
		}},
		&podKill{scope: podScope{component: "decode"}, entry: catalog.Entry{
			Type: "dynamo-worker-kill", Category: "dynamo", Summary: "Kill Dynamo decode worker pod(s)",
			Effect:   "The worker disappears mid-flight; in-flight requests fail or migrate (frontend --migration-limit), the router drops the worker, the operator recreates the pod and it re-registers.",
			Recovery: "Recovered when the graph has as many Ready worker pods as before.",
			Targets:  podTargets, Params: killParams(),
			References: []string{refDynamoFT, "https://docs.nvidia.com/dynamo/kubernetes/fault-tolerance/request-migration"},
		}},
		&podKill{scope: podScope{component: "frontend"}, entry: catalog.Entry{
			Type: "dynamo-frontend-kill", Category: "dynamo", Summary: "Kill the Dynamo frontend pod(s)",
			Effect:   "The OpenAI-compatible endpoint goes away until the operator recreates the frontend, which then rediscovers workers.",
			Recovery: "Recovered when as many frontend pods are Ready as before.",
			Targets:  podTargets, Params: killParams(), References: []string{refDynamoFT},
		}},
		&podKill{scope: podScope{namespace: "nvidia-sim-dynamo-system", selector: map[string]string{"app.kubernetes.io/name": "dynamo-operator"}}, entry: catalog.Entry{
			Type: "dynamo-operator-restart", Category: "dynamo", Summary: "Kill the Dynamo operator",
			Effect:   "DynamoGraphDeployments stop being reconciled until the operator is back; running graph pods are unaffected.",
			Recovery: "Recovered when the operator pod is Ready again.",
			Targets:  podTargets, Params: killParams(),
		}},
		&podKill{scope: podScope{namespace: "nvidia-sim-gpu-operator", selector: map[string]string{"app": "nvidia-device-plugin-daemonset"}}, entry: catalog.Entry{
			Type: "gpu-device-plugin-restart", Category: "gpu", Summary: "Kill the NVIDIA device plugin",
			Effect:   "The node's nvidia.com/gpu capacity is re-registered with the kubelet; new GPU pods cannot schedule meanwhile, running pods keep their GPUs.",
			Recovery: "Recovered when the device plugin pod is Ready again.",
			Targets:  podTargets, Params: killParams(),
			Requires: []string{"GPU Operator installed — the nvidia-sim gpuOperator component"},
		}},

		&netIsolate{scope: podScope{}, entry: catalog.Entry{
			Type: "network-isolate", Category: "network", Summary: "Cut pods off the network with a NetworkPolicy",
			Effect:   "A deny-all NetworkPolicy for the matched pods (ingress, egress or both; DNS kept by default).",
			Recovery: "The NetworkPolicy is deleted; recovered when it is gone and the pods are Ready.",
			Targets:  podTargets, Params: isolateParams(),
			Requires: []string{"a CNI that enforces NetworkPolicy (k3s does by default)"},
		}},
		&netIsolate{scope: podScope{component: "decode"}, entry: catalog.Entry{
			Type: "dynamo-worker-isolate", Category: "dynamo", Summary: "Partition the decode workers from the frontend and control plane",
			Effect:   "Requests routed to the worker time out; with egress cut the worker also loses Kubernetes discovery and is dropped from routing (Dynamo's FRONTEND_WORKER partition).",
			Recovery: "NetworkPolicy deleted; recovered when the workers are Ready.",
			Targets:  podTargets, Params: isolateParams(),
			Requires: []string{"a CNI that enforces NetworkPolicy (k3s does by default)"}, References: []string{"https://docs.nvidia.com/dynamo/knowledge-base/concepts/fault-tolerance/fault-tolerance-testing"},
		}},
		&netIsolate{scope: podScope{component: "frontend"}, entry: catalog.Entry{
			Type: "dynamo-frontend-isolate", Category: "dynamo", Summary: "Partition the frontend",
			Effect:   "Clients cannot reach the frontend (ingress) and/or the frontend cannot reach workers (egress).",
			Recovery: "NetworkPolicy deleted; recovered when the frontend is Ready.",
			Targets:  podTargets, Params: isolateParams(),
			Requires: []string{"a CNI that enforces NetworkPolicy (k3s does by default)"},
		}},

		&nodeCordon{entry: catalog.Entry{
			Type: "node-cordon", Category: "kubernetes", Summary: "Cordon nodes",
			Effect:   "Nodes become unschedulable; combine with a kill to see replacement pods stay Pending (on a single node the whole env).",
			Recovery: "Nodes this simulation cordoned are uncordoned (nodes already cordoned are left alone).",
			Targets:  []string{"node", "nodeSelector", "count"},
		}},
	}
}

// All returns every simulation type (nvidia-sim and runai-sim).
func All() []Injector {
	all := append(GPUInjectors(), KubernetesInjectors()...)
	all = append(all, RunaiInjectors()...)
	all = append(all, FaultInjectors()...)
	return append(all, observeOnly{})
}
