package inject

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	simv1 "github.com/benmaguire/onit-ai-testenv/simulations/controller/api/v1alpha1"
	"github.com/benmaguire/onit-ai-testenv/simulations/controller/internal/kube"
)

type fakeEph struct {
	runs []kube.EphemeralSpec
	pods []string
}

func (f *fakeEph) Run(_ context.Context, ns, pod string, spec kube.EphemeralSpec, _ time.Duration) (kube.EphemeralResult, error) {
	f.runs = append(f.runs, spec)
	f.pods = append(f.pods, ns+"/"+pod)
	return kube.EphemeralResult{}, nil
}

func appPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "app", UID: types.UID("u-" + name), Labels: map[string]string{"app": "api"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "api"}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, Conditions: ready()},
	}
}

func setupFault(t *testing.T) (*Env, *fakeEph, *fakeExec) {
	t.Helper()
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "app"},
		Spec: corev1.ServiceSpec{ClusterIP: "10.43.0.10", ClusterIPs: []string{"10.43.0.10"}, Selector: map[string]string{"app": "db"}}}
	eps := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "db-x", Namespace: "app", Labels: map[string]string{discoveryv1.LabelServiceName: "db"}},
		AddressType: discoveryv1.AddressTypeIPv4, Endpoints: []discoveryv1.Endpoint{{Addresses: []string{"10.42.0.7"}}}}
	db := appPod("db-0")
	db.Labels = map[string]string{"app": "db"}
	env, fe := testEnv(appPod("api-0"), appPod("api-1"), svc, eps, db)
	eph := &fakeEph{}
	env.Ephemeral = eph
	return env, eph, fe
}

func injectRecover(t *testing.T, env *Env, s *simv1.Simulation) Result {
	t.Helper()
	inj, ok := NewRegistry(All()...).Get(s.Spec.Type)
	if !ok {
		t.Fatalf("no type %s", s.Spec.Type)
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

func appSim(typ string, params map[string]string) *simv1.Simulation {
	return sim(typ, simv1.Target{Namespace: "app", PodSelector: map[string]string{"app": "api"}}, params)
}

func TestNetLatencyWholeInterface(t *testing.T) {
	env, eph, _ := setupFault(t)
	res := injectRecover(t, env, appSim("net-jitter", nil))
	if len(res.Targets) != 2 || len(eph.runs) != 4 {
		t.Fatalf("targets=%d runs=%d", len(res.Targets), len(eph.runs))
	}
	inj := strings.Join(eph.runs[0].Command, " ")
	if !strings.Contains(inj, "tc qdisc replace dev $IF root handle 1: netem delay 50ms 40ms 25% distribution normal") {
		t.Fatalf("inject script:\n%s", inj)
	}
	if eph.runs[0].Image != DefaultToolsImage || eph.runs[0].Capabilities[0] != "NET_ADMIN" {
		t.Fatalf("spec %+v", eph.runs[0])
	}
	if rec := strings.Join(eph.runs[2].Command, " "); !strings.Contains(rec, "tc qdisc del dev $IF root") {
		t.Fatalf("recover script:\n%s", rec)
	}
}

func TestNetLossTowardsServiceAndPort(t *testing.T) {
	env, eph, _ := setupFault(t)
	injectRecover(t, env, appSim("net-packet-loss", map[string]string{"loss": "30", "destinations": "svc/db", "ports": "5432"}))
	s := strings.Join(eph.runs[0].Command, " ")
	for _, want := range []string{"prio bands 4", "parent 1:4 handle 40: netem loss 30%",
		"match ip dst 10.42.0.7/32 match ip dport 5432 0xffff flowid 1:4", "match ip dst 10.43.0.10/32 match ip dport 5432"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in:\n%s", want, s)
		}
	}
}

func TestDependencyOutageAndDNS(t *testing.T) {
	env, eph, _ := setupFault(t)
	s := appSim("dependency-outage", map[string]string{"destinations": "svc/db,192.168.1.0/24", "mode": "drop"})
	injectRecover(t, env, s)
	inj := strings.Join(eph.runs[0].Command, " ")
	if !strings.Contains(inj, `iptables -I OUTPUT -d 10.43.0.10/32 -p tcp -m comment --comment "sim-s1" -j DROP`) ||
		!strings.Contains(inj, "-d 192.168.1.0/24") {
		t.Fatalf("inject:\n%s", inj)
	}
	if rec := strings.Join(eph.runs[2].Command, " "); !strings.Contains(rec, `grep -v -- "sim-s1"`) {
		t.Fatalf("recover:\n%s", rec)
	}
	eph.runs = nil
	injectRecover(t, env, appSim("dns-outage", nil))
	inj = strings.Join(eph.runs[0].Command, " ")
	if !strings.Contains(inj, "-p tcp -m multiport --dports 53") || !strings.Contains(inj, "-p udp -m multiport --dports 53") || !strings.Contains(inj, "-j DROP") {
		t.Fatalf("dns inject:\n%s", inj)
	}
}

func TestStressIsLongRunningAndStoppedByExec(t *testing.T) {
	env, eph, fe := setupFault(t)
	s := appSim("cpu-stress", map[string]string{"workers": "2", "load": "50"})
	s.Spec.Duration = &metav1.Duration{Duration: time.Minute}
	injectRecover(t, env, s)
	if len(eph.runs) != 2 {
		t.Fatalf("stress should add one container per pod, got %d", len(eph.runs))
	}
	if !strings.Contains(strings.Join(eph.runs[0].Command, " "), "load = 50 / 100.0") {
		t.Fatalf("script: %s", eph.runs[0].Command)
	}
	found := false
	for _, c := range fe.calls {
		if strings.Contains(c, "["+eph.runs[0].Name+"] bash -c kill -TERM 1") {
			found = true
		}
	}
	if !found {
		t.Fatalf("recover did not exec into the stress container: %v", fe.calls)
	}
}

func TestOverlappingNetemRefused(t *testing.T) {
	env, _, _ := setupFault(t)
	active := appSim("net-latency", nil)
	active.Name, active.UID = "other", "other-uid"
	active.Status = simv1.SimulationStatus{Phase: simv1.PhaseActive, Data: map[string]string{"group": "tc"},
		Targets: []simv1.AffectedTarget{{Kind: "Pod", Namespace: "app", Name: "api-0"}}}
	if err := env.Client.Create(context.Background(), active); err != nil {
		t.Fatal(err)
	}
	if err := env.Client.Status().Update(context.Background(), active); err != nil {
		t.Fatal(err)
	}
	inj, _ := NewRegistry(All()...).Get("net-packet-loss")
	_, err := inj.Inject(context.Background(), env, appSim("net-packet-loss", nil))
	if err == nil || !strings.Contains(err.Error(), "already has an active tc fault") {
		t.Fatalf("want overlap error, got %v", err)
	}
}

func TestServiceOutage(t *testing.T) {
	env, _, _ := setupFault(t)
	s := sim("service-outage", simv1.Target{Namespace: "app"}, map[string]string{"service": "db"})
	injectRecover(t, env, s)
	var np networkingv1.NetworkPolicyList
	_ = env.Client.List(context.Background(), &np)
	if len(np.Items) != 0 {
		t.Fatal("policy not removed on recover")
	}
	if s.Status.Data[dataSelector] != "app=db" {
		t.Fatalf("selector %q", s.Status.Data[dataSelector])
	}
}

func TestContainerKillVerifiesRestart(t *testing.T) {
	env, eph, _ := setupFault(t)
	s := appSim("container-kill", nil)
	s.Spec.Target.Count = 1
	injectRecover(t, env, s)
	if eph.runs[0].TargetContainer != "api" || strings.Join(eph.runs[0].Command, " ") != "bash -c kill -TERM 1" {
		t.Fatalf("spec %+v", eph.runs[0])
	}
	inj, _ := NewRegistry(All()...).Get("container-kill")
	if done, _, _ := inj.Verify(context.Background(), env, s); done {
		t.Fatal("verified without a restart")
	}
	var p corev1.Pod
	_ = env.Client.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "api-0"}, &p)
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "api", RestartCount: 1}}
	_ = env.Client.Status().Update(context.Background(), &p)
	if done, msg, _ := inj.Verify(context.Background(), env, s); !done {
		t.Fatalf("not verified after restart: %s", msg)
	}
}

func TestFaultValidation(t *testing.T) {
	r := NewRegistry(All()...)
	bad := []struct {
		typ string
		p   map[string]string
	}{
		{"net-latency", map[string]string{"delay": "fast"}},
		{"net-degraded", nil},
		{"net-packet-loss", map[string]string{"loss": "150"}},
		{"net-bandwidth", map[string]string{"rate": "lots"}},
		{"net-latency", map[string]string{"destinations": "not-an-ip"}},
		{"net-latency", map[string]string{"interface": "eth0; rm -rf /"}},
		{"dependency-outage", nil},
		{"memory-stress", map[string]string{"size": "huge"}},
		{"container-kill", map[string]string{"signal": "KILL"}},
		{"link-down", nil},
	}
	for _, c := range bad {
		inj, _ := r.Get(c.typ)
		if err := inj.Validate(appSim(c.typ, c.p)); err == nil {
			t.Errorf("%s %v: expected error", c.typ, c.p)
		}
	}
}
