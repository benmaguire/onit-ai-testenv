package inject

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simv1 "github.com/benmaguire/onit-ai-testenv/simulations/nvidia-sim/api/v1alpha1"
)

// fakeExec records commands and answers nvidia-smi queries.
type fakeExec struct {
	mu    sync.Mutex
	calls []string
	fail  map[string]error // substring -> error
	out   map[string]string
}

func (f *fakeExec) Exec(_ context.Context, ns, pod, container string, cmd []string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	line := fmt.Sprintf("%s/%s[%s] %s", ns, pod, container, strings.Join(cmd, " "))
	f.calls = append(f.calls, line)
	for k, err := range f.fail {
		if strings.Contains(line, k) {
			return "", "", err
		}
	}
	for k, o := range f.out {
		if strings.Contains(line, k) {
			return o, "", nil
		}
	}
	return "", "", nil
}

func (f *fakeExec) ctlCalls() []string {
	var out []string
	for _, c := range f.calls {
		if strings.Contains(c, mokkaCtl) {
			out = append(out, c[strings.Index(c, mokkaCtl)+len(mokkaCtl)+1:])
		}
	}
	return out
}

func scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = simv1.AddToScheme(s)
	return s
}

func ready() []corev1.PodCondition {
	return []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
}

func mokkaPod(node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "nvml-mock-" + node, Namespace: "nvidia-sim-mokka", Labels: map[string]string{"app.kubernetes.io/name": "nvml-mock"}},
		Spec:       corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "node-agent"}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, Conditions: ready()},
	}
}

func workerPod(name, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "nvidia-sim", UID: types.UID("uid-" + name), Labels: map[string]string{
			labelGraph: "mocker-agg", labelComponent: "decode",
		}},
		Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "main", Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{resourceGPU: resource.MustParse("1")},
		}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: ready()},
	}
}

func testEnv(objs ...client.Object) (*Env, *fakeExec) {
	fe := &fakeExec{fail: map[string]error{}, out: map[string]string{
		"nvml-mock-node1[node-agent] nvidia-smi --query-gpu=index,uuid":         "0, GPU-aaa\n1, GPU-bbb\n",
		"nvml-mock-node1[node-agent] nvidia-smi --query-gpu=index,memory.total": "0, 81559\n1, 81559\n",
		"worker-1[main] nvidia-smi --query-gpu=uuid":                            "GPU-bbb\n",
	}}
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(objs...).Build()
	return &Env{
		Client: c, Exec: fe, Log: logr.Discard(),
		MokkaNamespace: "nvidia-sim-mokka", MokkaSelector: map[string]string{"app.kubernetes.io/name": "nvml-mock"},
		DefaultGraph: "mocker-agg", DefaultGraphNamespace: "nvidia-sim", Now: time.Now,
	}, fe
}

func sim(typ string, target simv1.Target, params map[string]string) *simv1.Simulation {
	return &simv1.Simulation{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "sim-system", UID: "sim-uid"},
		Spec:       simv1.SimulationSpec{Type: typ, Target: target, Params: params},
	}
}

func run(t *testing.T, env *Env, s *simv1.Simulation) Result {
	t.Helper()
	inj, ok := NewRegistry(All()...).Get(s.Spec.Type)
	if !ok {
		t.Fatalf("type %s not registered", s.Spec.Type)
	}
	if err := inj.Validate(s); err != nil {
		t.Fatalf("validate: %v", err)
	}
	res, err := inj.Inject(context.Background(), env, s)
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	s.Status.Targets, s.Status.Data = res.Targets, res.Data
	if _, err := inj.Recover(context.Background(), env, s); err != nil {
		t.Fatalf("recover: %v", err)
	}
	return res
}

func TestCatalogComplete(t *testing.T) {
	cat := NewRegistry(All()...).Catalog()
	if len(cat) < 20 {
		t.Fatalf("expected at least 20 types, got %d", len(cat))
	}
	for _, e := range cat {
		if e.Summary == "" || e.Effect == "" || e.Recovery == "" || e.Category == "" || len(e.Targets) == 0 {
			t.Errorf("%s: incomplete catalog entry %+v", e.Type, e)
		}
	}
}

func TestGPUXidOnNode(t *testing.T) {
	env, fe := testEnv(mokkaPod("node1"))
	s := sim("gpu-xid", simv1.Target{Node: "node1", GPU: "0"}, map[string]string{"xid": "79", "mode": "fallen_off_bus"})
	run(t, env, s)
	got := fe.ctlCalls()
	want := []string{"fail --gpu 0 --mode fallen_off_bus --xid 79", "fail --gpu 0 --mode healthy"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %q, want %q", got, want)
	}
}

func TestGPUFaultOnWorkerPodsGPU(t *testing.T) {
	env, fe := testEnv(mokkaPod("node1"), workerPod("worker-1", "node1"))
	s := sim("gpu-ecc-uncorrectable", simv1.Target{Graph: "mocker-agg", Component: "decode", Namespace: "nvidia-sim"}, nil)
	res := run(t, env, s)
	if len(res.Targets) != 1 || res.Targets[0].GPU != "1" || res.Targets[0].Detail != "nvidia-sim/worker-1" {
		t.Fatalf("targets = %+v, want GPU 1 via worker-1", res.Targets)
	}
	if got := fe.ctlCalls()[0]; got != "fail --gpu 1 --mode ecc_uncorrectable --xid 48" {
		t.Fatalf("inject call = %q", got)
	}
}

func TestGPUOverheatDefaultsAndReset(t *testing.T) {
	env, fe := testEnv(mokkaPod("node1"))
	run(t, env, sim("gpu-overheat", simv1.Target{}, nil))
	want := []string{"temp --gpu all 95", "throttle --gpu all thermal", "reset --gpu all"}
	if got := fe.ctlCalls(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %q, want %q", got, want)
	}
}

func TestGPUMemoryPressureComputesBytes(t *testing.T) {
	env, fe := testEnv(mokkaPod("node1"))
	run(t, env, sim("gpu-memory-pressure", simv1.Target{GPU: "0"}, map[string]string{"usedPercent": "50"}))
	total := uint64(81559) << 20
	want := fmt.Sprintf("set --gpu 0 memory.used_bytes=%d memory.free_bytes=%d", total/2, total-total/2)
	if got := fe.ctlCalls()[0]; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestValidation(t *testing.T) {
	r := NewRegistry(All()...)
	cases := []struct {
		typ    string
		target simv1.Target
		params map[string]string
	}{
		{"gpu-xid", simv1.Target{}, map[string]string{"mode": "melted"}},
		{"gpu-overheat", simv1.Target{}, map[string]string{"temperature": "900"}},
		{"gpu-overheat", simv1.Target{}, map[string]string{"temprature": "90"}}, // typo
		{"gpu-throttle", simv1.Target{}, map[string]string{"reasons": "bogus"}},
		{"gpu-xid", simv1.Target{GPU: "first"}, nil},
		{"pod-kill", simv1.Target{}, nil}, // needs a selector
		{"network-isolate", simv1.Target{PodSelector: map[string]string{"a": "b"}}, map[string]string{"direction": "sideways"}},
		{"gpu-set", simv1.Target{}, nil}, // fields required
	}
	for _, c := range cases {
		inj, _ := r.Get(c.typ)
		if err := inj.Validate(sim(c.typ, c.target, c.params)); err == nil {
			t.Errorf("%s %+v %v: expected validation error", c.typ, c.target, c.params)
		}
	}
}

func TestWorkerKillAndVerify(t *testing.T) {
	env, _ := testEnv(workerPod("worker-1", "node1"), workerPod("worker-2", "node1"))
	s := sim("dynamo-worker-kill", simv1.Target{Count: 1}, nil)
	res := run(t, env, s)
	if len(res.Targets) != 1 || res.Targets[0].Name != "worker-1" {
		t.Fatalf("targets = %+v", res.Targets)
	}
	inj, _ := NewRegistry(All()...).Get("dynamo-worker-kill")
	done, msg, err := inj.Verify(context.Background(), env, s)
	if err != nil || done {
		t.Fatalf("verify right after kill: done=%v msg=%q err=%v (want not done: 1/2 ready)", done, msg, err)
	}
	// The operator replaces the pod.
	if err := env.Client.Create(context.Background(), workerPod("worker-3", "node1")); err != nil {
		t.Fatal(err)
	}
	done, msg, _ = inj.Verify(context.Background(), env, s)
	if !done {
		t.Fatalf("verify after replacement: %s", msg)
	}
}

func TestNetworkIsolateCreatesAndRemovesPolicy(t *testing.T) {
	env, _ := testEnv(workerPod("worker-1", "node1"))
	s := sim("dynamo-worker-isolate", simv1.Target{}, map[string]string{"direction": "egress"})
	inj, _ := NewRegistry(All()...).Get(s.Spec.Type)
	res, err := inj.Inject(context.Background(), env, s)
	if err != nil {
		t.Fatal(err)
	}
	var np networkingv1.NetworkPolicy
	if err := env.Client.Get(context.Background(), types.NamespacedName{Namespace: "nvidia-sim", Name: "sim-s1"}, &np); err != nil {
		t.Fatalf("policy not created: %v", err)
	}
	if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress || len(np.Spec.Egress) != 1 {
		t.Fatalf("unexpected policy %+v", np.Spec)
	}
	s.Status.Targets, s.Status.Data = res.Targets, res.Data
	if _, err := inj.Recover(context.Background(), env, s); err != nil {
		t.Fatal(err)
	}
	if done, msg, _ := inj.Verify(context.Background(), env, s); !done {
		t.Fatalf("not recovered: %s", msg)
	}
}

func TestNodeCordonLeavesPreCordonedNodes(t *testing.T) {
	n1 := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: map[string]string{"pool": "a"}}}
	n2 := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n2", Labels: map[string]string{"pool": "a"}}, Spec: corev1.NodeSpec{Unschedulable: true}}
	env, _ := testEnv(n1, n2)
	s := sim("node-cordon", simv1.Target{NodeSelector: map[string]string{"pool": "a"}}, nil)
	res := run(t, env, s)
	if len(res.Targets) != 1 || res.Targets[0].Name != "n1" {
		t.Fatalf("targets = %+v", res.Targets)
	}
	var got corev1.Node
	_ = env.Client.Get(context.Background(), types.NamespacedName{Name: "n2"}, &got)
	if !got.Spec.Unschedulable {
		t.Fatal("pre-cordoned node was uncordoned")
	}
}

func TestMissingMokkaIsAnError(t *testing.T) {
	env, _ := testEnv()
	inj, _ := NewRegistry(All()...).Get("gpu-lost")
	if _, err := inj.Inject(context.Background(), env, sim("gpu-lost", simv1.Target{Node: "node1"}, nil)); err == nil {
		t.Fatal("expected error when Mokka is not running")
	}
}
