package inject

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	simv1 "github.com/benmaguire/onit-ai-testenv/simulations/controller/api/v1alpha1"
)

func kwokNode(name string, ready bool) *corev1.Node {
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"type": "kwok", "run.ai/simulated-gpu-node-pool": "h100"},
			Annotations: map[string]string{annotationKwokNode: kwokManagedValue}},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: st}}},
	}
}

func annotation(t *testing.T, env *Env, name string) string {
	t.Helper()
	var n corev1.Node
	if err := env.Client.Get(context.Background(), types.NamespacedName{Name: name}, &n); err != nil {
		t.Fatal(err)
	}
	return n.Annotations[annotationKwokNode]
}

func TestKwokNodeDown(t *testing.T) {
	real := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "k3s-server", Labels: map[string]string{"type": "kwok"}}} // mislabelled real node
	env, _ := testEnv(kwokNode("v-0", true), kwokNode("v-1", true), real)
	s := sim("kwok-node-down", simv1.Target{Count: 1}, nil)
	inj, _ := NewRegistry(All()...).Get("kwok-node-down")
	res, err := inj.Inject(context.Background(), env, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Targets) != 1 || res.Targets[0].Name != "v-0" || annotation(t, env, "v-0") != kwokPausedValue {
		t.Fatalf("targets %+v annotation %q", res.Targets, annotation(t, env, "v-0"))
	}
	if annotation(t, env, "v-1") != kwokManagedValue {
		t.Fatal("count=1 paused more than one node")
	}
	s.Status.Targets = res.Targets
	if _, err := inj.Recover(context.Background(), env, s); err != nil {
		t.Fatal(err)
	}
	if annotation(t, env, "v-0") != kwokManagedValue {
		t.Fatal("heartbeat annotation not restored")
	}
	if done, _, _ := inj.Verify(context.Background(), env, s); !done {
		t.Fatal("ready node not verified")
	}
	// A real node is refused even when named explicitly.
	if _, err := inj.Inject(context.Background(), env, sim("kwok-node-down", simv1.Target{Node: "k3s-server"}, nil)); err == nil {
		t.Fatal("real node was not refused")
	}
}

func TestKwokNodeDownVerifyWaitsForReady(t *testing.T) {
	env, _ := testEnv(kwokNode("v-0", false))
	s := sim("kwok-node-down", simv1.Target{}, nil)
	s.Status.Targets = []simv1.AffectedTarget{{Kind: "Node", Name: "v-0"}}
	inj, _ := NewRegistry(All()...).Get("kwok-node-down")
	if done, _, _ := inj.Verify(context.Background(), env, s); done {
		t.Fatal("NotReady node verified as recovered")
	}
}

func TestFGOUtilizationRestoresOriginals(t *testing.T) {
	p1 := workerPod("job-0", "v-0")
	p1.Namespace = "runai-sim"
	p1.Labels = map[string]string{"app": "train"}
	p1.Annotations = map[string]string{annotationUtilization: "70-90"}
	p2 := p1.DeepCopy()
	p2.Name, p2.Annotations = "job-1", nil
	env, _ := testEnv(p1, p2)
	s := sim("fgo-gpu-utilization", simv1.Target{Namespace: "runai-sim", PodSelector: map[string]string{"app": "train"}}, map[string]string{"range": "0"})
	res := run(t, env, s)
	if len(res.Targets) != 2 {
		t.Fatalf("targets %+v", res.Targets)
	}
	get := func(name string) (string, bool) {
		var p corev1.Pod
		_ = env.Client.Get(context.Background(), types.NamespacedName{Namespace: "runai-sim", Name: name}, &p)
		v, ok := p.Annotations[annotationUtilization]
		return v, ok
	}
	if v, _ := get("job-0"); v != "70-90" {
		t.Fatalf("job-0 annotation %q, want restored 70-90", v)
	}
	if _, ok := get("job-1"); ok {
		t.Fatal("job-1 annotation should be removed again")
	}
	inj, _ := NewRegistry(All()...).Get("fgo-gpu-utilization")
	if err := inj.Validate(sim("fgo-gpu-utilization", simv1.Target{PodSelector: map[string]string{"a": "b"}}, map[string]string{"range": "lots"})); err == nil {
		t.Fatal("bad range accepted")
	}
}
