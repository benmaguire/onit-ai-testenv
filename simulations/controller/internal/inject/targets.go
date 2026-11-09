package inject

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simv1 "github.com/benmaguire/onit-ai-testenv/simulations/controller/api/v1alpha1"
)

const (
	// Dynamo operator pod labels.
	labelGraph     = "nvidia.com/dynamo-graph-deployment-name"
	labelComponent = "nvidia.com/dynamo-component-type"
	// GPU resource name.
	resourceGPU = "nvidia.com/gpu"
)

// podSelectorFor builds the namespace and label selector for pod targets.
// Graph/Component expand to the Dynamo operator's pod labels.
func podSelectorFor(env *Env, sim *simv1.Simulation, defaultComponent string) (string, map[string]string) {
	t := sim.Spec.Target
	sel := map[string]string{}
	for k, v := range t.PodSelector {
		sel[k] = v
	}
	graph, comp := t.Graph, t.Component
	if defaultComponent != "" {
		if graph == "" && len(t.PodSelector) == 0 {
			graph = env.DefaultGraph
		}
		if comp == "" {
			comp = defaultComponent
		}
	}
	if graph != "" {
		sel[labelGraph] = graph
	}
	if comp != "" {
		sel[labelComponent] = comp
	}
	ns := t.Namespace
	if ns == "" {
		if defaultComponent != "" && env.DefaultGraphNamespace != "" {
			ns = env.DefaultGraphNamespace
		} else {
			ns = sim.Namespace
		}
	}
	return ns, sel
}

func hasPodTarget(sim *simv1.Simulation) bool {
	t := sim.Spec.Target
	return len(t.PodSelector) > 0 || t.Graph != "" || t.Component != ""
}

// listPods returns pods matching the selector, sorted by name, skipping pods
// that are already terminating.
func listPods(ctx context.Context, c client.Client, ns string, sel map[string]string) ([]corev1.Pod, error) {
	if len(sel) == 0 {
		return nil, fmt.Errorf("refusing to select every pod in %q: set target.podSelector, graph or component", ns)
	}
	var pl corev1.PodList
	if err := c.List(ctx, &pl, client.InNamespace(ns), client.MatchingLabelsSelector{Selector: labels.SelectorFromSet(sel)}); err != nil {
		return nil, err
	}
	out := pl.Items[:0]
	for _, p := range pl.Items {
		if p.DeletionTimestamp == nil {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out, nil
}

func limit[T any](items []T, n int32) []T {
	if n > 0 && int(n) < len(items) {
		return items[:n]
	}
	return items
}

// resolveNodes returns target node names: target.node, target.nodeSelector, or
// every node running a Mokka pod.
func resolveNodes(ctx context.Context, env *Env, sim *simv1.Simulation) ([]string, error) {
	t := sim.Spec.Target
	if t.Node != "" {
		return []string{t.Node}, nil
	}
	var names []string
	if len(t.NodeSelector) > 0 {
		var nl corev1.NodeList
		if err := env.Client.List(ctx, &nl, client.MatchingLabels(t.NodeSelector)); err != nil {
			return nil, err
		}
		for _, n := range nl.Items {
			names = append(names, n.Name)
		}
	} else {
		pods, err := mokkaPods(ctx, env)
		if err != nil {
			return nil, err
		}
		for _, p := range pods {
			if p.Spec.NodeName != "" {
				names = append(names, p.Spec.NodeName)
			}
		}
	}
	sort.Strings(names)
	names = limit(names, t.Count)
	if len(names) == 0 {
		return nil, fmt.Errorf("no target nodes found")
	}
	return names, nil
}

func mokkaPods(ctx context.Context, env *Env) ([]corev1.Pod, error) {
	var pl corev1.PodList
	if err := env.Client.List(ctx, &pl, client.InNamespace(env.MokkaNamespace), client.MatchingLabels(env.MokkaSelector)); err != nil {
		return nil, err
	}
	return pl.Items, nil
}

// mokkaPodOn returns the running nvml-mock pod on a node.
func mokkaPodOn(ctx context.Context, env *Env, node string) (*corev1.Pod, error) {
	pods, err := mokkaPods(ctx, env)
	if err != nil {
		return nil, err
	}
	for i := range pods {
		p := &pods[i]
		if p.Spec.NodeName == node && p.Status.Phase == corev1.PodRunning && p.DeletionTimestamp == nil {
			return p, nil
		}
	}
	return nil, fmt.Errorf("no running nvml-mock pod on node %q in namespace %q (is Mokka installed?)", node, env.MokkaNamespace)
}

// gpuTarget is one GPU (or "all") on one node.
type gpuTarget struct {
	Node  string
	GPU   string
	Mokka *corev1.Pod
	Via   string // pod that holds the GPU, when resolved from pods
}

// resolveGPUs finds GPU targets: GPUs allocated to the target pods, or
// target.gpu on the target nodes.
func resolveGPUs(ctx context.Context, env *Env, sim *simv1.Simulation) ([]gpuTarget, error) {
	if hasPodTarget(sim) {
		return resolvePodGPUs(ctx, env, sim)
	}
	nodes, err := resolveNodes(ctx, env, sim)
	if err != nil {
		return nil, err
	}
	gpu := sim.Spec.Target.GPU
	if gpu == "" {
		gpu = "all"
	}
	var out []gpuTarget
	for _, n := range nodes {
		mp, err := mokkaPodOn(ctx, env, n)
		if err != nil {
			return nil, err
		}
		out = append(out, gpuTarget{Node: n, GPU: gpu, Mokka: mp})
	}
	return out, nil
}

// resolvePodGPUs maps each target pod's GPU UUIDs (from nvidia-smi in the pod)
// to device indexes on its node (from nvidia-smi in the Mokka pod).
func resolvePodGPUs(ctx context.Context, env *Env, sim *simv1.Simulation) ([]gpuTarget, error) {
	ns, sel := podSelectorFor(env, sim, "")
	pods, err := listPods(ctx, env.Client, ns, sel)
	if err != nil {
		return nil, err
	}
	pods = limit(runningGPUPods(pods), sim.Spec.Target.Count)
	if len(pods) == 0 {
		return nil, fmt.Errorf("no running pods holding %s match %v in %q", resourceGPU, sel, ns)
	}
	indexCache := map[string]map[string]string{} // node -> uuid -> index
	var out []gpuTarget
	for i := range pods {
		p := &pods[i]
		mp, err := mokkaPodOn(ctx, env, p.Spec.NodeName)
		if err != nil {
			return nil, err
		}
		idx, ok := indexCache[p.Spec.NodeName]
		if !ok {
			stdout, _, err := env.Exec.Exec(ctx, mp.Namespace, mp.Name, mokkaContainer,
				[]string{"nvidia-smi", "--query-gpu=index,uuid", "--format=csv,noheader"})
			if err != nil {
				return nil, fmt.Errorf("list GPUs on %s: %w", p.Spec.NodeName, err)
			}
			idx = map[string]string{}
			for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
				f := strings.Split(line, ",")
				if len(f) == 2 {
					idx[strings.TrimSpace(f[1])] = strings.TrimSpace(f[0])
				}
			}
			indexCache[p.Spec.NodeName] = idx
		}
		stdout, _, err := env.Exec.Exec(ctx, p.Namespace, p.Name, gpuContainer(p),
			[]string{"nvidia-smi", "--query-gpu=uuid", "--format=csv,noheader"})
		if err != nil {
			return nil, fmt.Errorf("list GPUs in pod %s/%s: %w", p.Namespace, p.Name, err)
		}
		for _, uuid := range strings.Fields(stdout) {
			i, ok := idx[uuid]
			if !ok {
				return nil, fmt.Errorf("pod %s/%s GPU %s not found on node %s", p.Namespace, p.Name, uuid, p.Spec.NodeName)
			}
			out = append(out, gpuTarget{Node: p.Spec.NodeName, GPU: i, Mokka: mp, Via: p.Namespace + "/" + p.Name})
		}
	}
	return out, nil
}

func runningGPUPods(pods []corev1.Pod) []corev1.Pod {
	var out []corev1.Pod
	for _, p := range pods {
		if p.Status.Phase == corev1.PodRunning && gpuContainer(&p) != "" {
			out = append(out, p)
		}
	}
	return out
}

// gpuContainer returns the first container requesting a GPU.
func gpuContainer(p *corev1.Pod) string {
	for _, c := range p.Spec.Containers {
		if q, ok := c.Resources.Limits[resourceGPU]; ok && !q.IsZero() {
			return c.Name
		}
	}
	return ""
}

// podsReady counts Ready pods.
func podsReady(pods []corev1.Pod) int {
	n := 0
	for _, p := range pods {
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				n++
			}
		}
	}
	return n
}
