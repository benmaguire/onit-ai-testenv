package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simv1 "github.com/benmaguire/onit-ai-testenv/simulations/controller/api/v1alpha1"
	"github.com/benmaguire/onit-ai-testenv/simulations/controller/internal/inject"
	"github.com/benmaguire/onit-ai-testenv/simulations/controller/internal/observe"
)

type recExec struct{ cmds []string }

func (r *recExec) Exec(_ context.Context, _, _, _ string, cmd []string) (string, string, error) {
	r.cmds = append(r.cmds, strings.Join(cmd, " "))
	return "", "", nil
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func setup(t *testing.T, sim *simv1.Simulation) (*SimulationReconciler, client.Client, *recExec, *clock) {
	t.Helper()
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = simv1.AddToScheme(s)
	mokka := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "nvml-mock-x", Namespace: "mokka", Labels: map[string]string{"app": "nvml-mock"}},
		Spec:       corev1.PodSpec{NodeName: "node1"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(sim, mokka).WithStatusSubresource(&simv1.Simulation{}).Build()
	ex := &recExec{}
	clk := &clock{t: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	env := &inject.Env{Client: c, Exec: ex, Log: logr.Discard(), MokkaNamespace: "mokka", MokkaSelector: map[string]string{"app": "nvml-mock"}}
	return &SimulationReconciler{Client: c, Env: env, Registry: inject.NewRegistry(inject.All()...), Now: clk.now}, c, ex, clk
}

func reconcile(t *testing.T, r *SimulationReconciler, c client.Client) (*simv1.Simulation, ctrl.Result) {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "s"}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := &simv1.Simulation{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "s"}, got); err != nil {
		return nil, res
	}
	return got, res
}

func newSim(spec simv1.SimulationSpec) *simv1.Simulation {
	return &simv1.Simulation{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"}, Spec: spec}
}

func TestLifecycleWithDuration(t *testing.T) {
	r, c, ex, clk := setup(t, newSim(simv1.SimulationSpec{
		Type: "gpu-lost", Target: simv1.Target{Node: "node1", GPU: "0"},
		Duration: &metav1.Duration{Duration: time.Minute},
	}))
	got, res := reconcile(t, r, c)
	if got.Status.Phase != simv1.PhaseActive {
		t.Fatalf("phase = %s (%s), want Active", got.Status.Phase, got.Status.Message)
	}
	if res.RequeueAfter != time.Minute {
		t.Fatalf("requeue = %v, want 1m", res.RequeueAfter)
	}
	if len(got.Finalizers) != 1 {
		t.Fatalf("finalizer not added")
	}
	// Not due yet.
	clk.t = clk.t.Add(30 * time.Second)
	if got, _ = reconcile(t, r, c); got.Status.Phase != simv1.PhaseActive {
		t.Fatalf("recovered early: %s", got.Status.Phase)
	}
	clk.t = clk.t.Add(31 * time.Second)
	if got, _ = reconcile(t, r, c); got.Status.Phase != simv1.PhaseRecovering {
		t.Fatalf("phase = %s, want Recovering", got.Status.Phase)
	}
	if got, _ = reconcile(t, r, c); got.Status.Phase != simv1.PhaseRecovered {
		t.Fatalf("phase = %s (%s), want Recovered", got.Status.Phase, got.Status.Message)
	}
	if got.Status.Data["timeToRecover"] != "1m1s" {
		t.Fatalf("timeToRecover = %q", got.Status.Data["timeToRecover"])
	}
	joined := strings.Join(ex.cmds, "\n")
	if !strings.Contains(joined, "fail --gpu 0 --mode lost") || !strings.Contains(joined, "fail --gpu 0 --mode healthy") {
		t.Fatalf("commands:\n%s", joined)
	}
}

func TestManualRecoverAndDeleteReverts(t *testing.T) {
	r, c, ex, _ := setup(t, newSim(simv1.SimulationSpec{Type: "gpu-overheat", Target: simv1.Target{Node: "node1"}}))
	got, res := reconcile(t, r, c)
	if got.Status.Phase != simv1.PhaseActive || res.RequeueAfter != 0 {
		t.Fatalf("phase=%s requeue=%v; want held Active", got.Status.Phase, res.RequeueAfter)
	}
	// Deleting an active simulation must revert the fault.
	if err := c.Delete(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	if got, _ = reconcile(t, r, c); got != nil {
		t.Fatalf("simulation still present after finalizer: %+v", got.Finalizers)
	}
	if last := ex.cmds[len(ex.cmds)-1]; !strings.HasSuffix(last, "reset --gpu all") {
		t.Fatalf("last command %q, want reset", last)
	}
}

func TestDelayAndCancel(t *testing.T) {
	sim := newSim(simv1.SimulationSpec{Type: "gpu-overheat", Delay: &metav1.Duration{Duration: time.Hour}})
	r, c, ex, clk := setup(t, sim)
	// fake client sets no creationTimestamp; anchor it to the clock.
	got := &simv1.Simulation{}
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(sim), got)
	got.CreationTimestamp = metav1.NewTime(clk.t)
	_ = c.Update(context.Background(), got)

	got, res := reconcile(t, r, c)
	if got.Status.Phase != simv1.PhaseWaiting || res.RequeueAfter != time.Hour {
		t.Fatalf("phase=%s requeue=%v; want Waiting 1h", got.Status.Phase, res.RequeueAfter)
	}
	got.Spec.Recover = true
	_ = c.Update(context.Background(), got)
	if got, _ = reconcile(t, r, c); got.Status.Phase != simv1.PhaseRecovered {
		t.Fatalf("phase=%s, want Recovered (cancelled)", got.Status.Phase)
	}
	if len(ex.cmds) != 0 {
		t.Fatalf("cancelled simulation ran commands: %v", ex.cmds)
	}
}

func TestInvalidAndUnknownFail(t *testing.T) {
	r, c, _, _ := setup(t, newSim(simv1.SimulationSpec{Type: "gpu-xid", Params: map[string]string{"xid": "abc"}}))
	if got, _ := reconcile(t, r, c); got.Status.Phase != simv1.PhaseFailed || !strings.Contains(got.Status.Message, "xid") {
		t.Fatalf("phase=%s msg=%q", got.Status.Phase, got.Status.Message)
	}
	r, c, _, _ = setup(t, newSim(simv1.SimulationSpec{Type: "meteor-strike"}))
	if got, _ := reconcile(t, r, c); got.Status.Phase != simv1.PhaseFailed {
		t.Fatalf("unknown type phase=%s", got.Status.Phase)
	}
}

func TestInjectFailureMarksFailed(t *testing.T) {
	// No Mokka pod on node2.
	r, c, _, _ := setup(t, newSim(simv1.SimulationSpec{Type: "gpu-lost", Target: simv1.Target{Node: "node2"}}))
	got, _ := reconcile(t, r, c)
	if got.Status.Phase != simv1.PhaseFailed || !strings.Contains(got.Status.Message, "nvml-mock") {
		t.Fatalf("phase=%s msg=%q", got.Status.Phase, got.Status.Message)
	}
}

func TestObservationLifecycle(t *testing.T) {
	r, c, _, clk := setup(t, newSim(simv1.SimulationSpec{
		Type: "gpu-lost", Target: simv1.Target{Node: "node1", GPU: "0"},
		Duration: &metav1.Duration{Duration: time.Minute},
		Observe: &simv1.ObserveSpec{
			Baseline: &metav1.Duration{Duration: time.Minute}, After: &metav1.Duration{Duration: 2 * time.Minute},
			Probes: []simv1.Probe{{Name: "tcp", Type: "tcp", Address: "127.0.0.1:1"}},
		},
	}))
	r.Observer = &observe.Observer{Profiles: observe.Builtin, Probes: &observe.ProbeManager{}, Now: clk.now}
	got, res := reconcile(t, r, c)
	if got.Status.Phase != simv1.PhaseBaselining || res.RequeueAfter != time.Minute || got.Status.Observation == nil {
		t.Fatalf("phase=%s requeue=%v obs=%v", got.Status.Phase, res.RequeueAfter, got.Status.Observation)
	}
	clk.t = clk.t.Add(61 * time.Second)
	if got, _ = reconcile(t, r, c); got.Status.Phase != simv1.PhaseActive {
		t.Fatalf("phase=%s (%s)", got.Status.Phase, got.Status.Message)
	}
	clk.t = clk.t.Add(61 * time.Second)
	reconcile(t, r, c) // -> Recovering
	got, res = reconcile(t, r, c)
	if got.Status.Phase != simv1.PhaseRecovered || got.Status.Observation.Complete || res.RequeueAfter != 2*time.Minute {
		t.Fatalf("phase=%s complete=%v requeue=%v", got.Status.Phase, got.Status.Observation.Complete, res.RequeueAfter)
	}
	clk.t = clk.t.Add(2*time.Minute + time.Second)
	got, _ = reconcile(t, r, c)
	if !got.Status.Observation.Complete || len(got.Status.Observation.Probes) != 1 {
		t.Fatalf("observation %+v", got.Status.Observation)
	}
	if w := got.Status.Observation.Probes[0].Windows; len(w) != 3 { // recovery verified instantly: zero-length window skipped
		t.Fatalf("windows %v", w)
	}
}

func TestInvalidObserveFails(t *testing.T) {
	r, c, _, _ := setup(t, newSim(simv1.SimulationSpec{Type: "gpu-lost", Observe: &simv1.ObserveSpec{Profiles: []string{"bogus"}}}))
	if got, _ := reconcile(t, r, c); got.Status.Phase != simv1.PhaseFailed || !strings.Contains(got.Status.Message, "bogus") {
		t.Fatalf("phase=%s msg=%s", got.Status.Phase, got.Status.Message)
	}
}
