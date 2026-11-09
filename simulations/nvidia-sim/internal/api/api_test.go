package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simv1 "github.com/benmaguire/onit-ai-testenv/simulations/nvidia-sim/api/v1alpha1"
	"github.com/benmaguire/onit-ai-testenv/simulations/nvidia-sim/internal/inject"
	apic "github.com/benmaguire/onit-ai-testenv/simulations/nvidia-sim/pkg/client"
)

func newClient(objs ...client.Object) client.Client {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = simv1.AddToScheme(s)
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&simv1.Simulation{}, &simv1.SimulationCluster{}).Build()
}

func newServer(name, token string, hub bool, objs ...client.Object) (*Server, *httptest.Server) {
	c := newClient(objs...)
	s := &Server{Client: c, Registry: inject.NewRegistry(inject.All()...), ClusterName: name, Version: "test",
		DefaultNamespace: "sim-system", Token: token, Log: logr.Discard()}
	if hub {
		s.Hub = &Hub{Client: c, Namespace: "sim-system"}
		s.RegistrationToken = "reg-secret"
	}
	return s, httptest.NewServer(s.Handler())
}

func create(t *testing.T, c *apic.Client, name string) *simv1.Simulation {
	t.Helper()
	s, err := c.Create(context.Background(), apic.CreateRequest{Name: name, Labels: map[string]string{"testrun": "t1"},
		Spec: simv1.SimulationSpec{Type: "gpu-overheat", Params: map[string]string{"temperature": "99"}}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return s
}

func TestAgentAPI(t *testing.T) {
	_, ts := newServer("lab", "secret", false)
	defer ts.Close()
	ctx := context.Background()

	// Auth required.
	if _, err := apic.New(ts.URL, "wrong").Catalog(ctx); !isStatus(err, 401) {
		t.Fatalf("want 401, got %v", err)
	}
	c := apic.New(ts.URL, "secret")
	cat, err := c.Catalog(ctx)
	if err != nil || len(cat) == 0 {
		t.Fatalf("catalog: %v %d", err, len(cat))
	}
	// Bad requests are rejected before anything is created.
	if _, err := c.Create(ctx, apic.CreateRequest{Spec: simv1.SimulationSpec{Type: "nope"}}); !isStatus(err, 400) {
		t.Fatalf("unknown type: want 400, got %v", err)
	}
	if _, err := c.Create(ctx, apic.CreateRequest{Spec: simv1.SimulationSpec{Type: "gpu-overheat", Params: map[string]string{"temperature": "hot"}}}); !isStatus(err, 400) {
		t.Fatalf("invalid param: want 400, got %v", err)
	}
	s := create(t, c, "overheat-1")
	if s.Namespace != "sim-system" {
		t.Fatalf("namespace = %q", s.Namespace)
	}
	got, err := c.Get(ctx, "sim-system", "overheat-1")
	if err != nil || got.Spec.Params["temperature"] != "99" {
		t.Fatalf("get: %v %+v", err, got)
	}
	if _, err := c.Recover(ctx, "sim-system", "overheat-1"); err != nil {
		t.Fatal(err)
	}
	got, _ = c.Get(ctx, "sim-system", "overheat-1")
	if !got.Spec.Recover {
		t.Fatal("recover flag not set")
	}
	list, err := c.List(ctx, apic.ListOptions{LabelSelector: "testrun=t1"})
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %d", err, len(list))
	}
	if err := c.Delete(ctx, "sim-system", "overheat-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(ctx, "sim-system", "overheat-1"); !isStatus(err, 404) {
		t.Fatalf("want 404, got %v", err)
	}
	// Health needs no token.
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("healthz: %v", err)
	}
}

func TestRecoverAll(t *testing.T) {
	_, ts := newServer("lab", "", false)
	defer ts.Close()
	c := apic.New(ts.URL, "")
	create(t, c, "a")
	create(t, c, "b")
	res, err := c.RecoverAll(context.Background(), "", "testrun=t1")
	if err != nil || len(res.Recovered) != 2 {
		t.Fatalf("recover-all: %v %+v", err, res)
	}
}

func TestHubRoutesToLocalAndRemoteAgents(t *testing.T) {
	ctx := context.Background()
	_, agent := newServer("lab-2", "agent-token", false)
	defer agent.Close()
	hubSrv, hub := newServer("central", "hub-token", true)
	defer hub.Close()

	// Agent self-registers with the registration token.
	reg := apic.New(hub.URL, "reg-secret")
	if err := reg.Do(ctx, http.MethodPost, hub.URL+"/api/v1/clusters/register",
		apic.RegisterRequest{Name: "lab-2", Endpoint: agent.URL, Token: "agent-token", Environment: "nvidia-sim"}, nil); err != nil {
		t.Fatalf("register: %v", err)
	}
	// The registration token cannot call anything else.
	if _, err := reg.Clusters(ctx); !isStatus(err, 401) {
		t.Fatalf("registration token should not list clusters: %v", err)
	}
	var sec corev1.Secret
	if err := hubSrv.Client.Get(ctx, types.NamespacedName{Namespace: "sim-system", Name: "simcluster-lab-2"}, &sec); err != nil || string(sec.Data["token"]) != "agent-token" {
		t.Fatalf("token secret: %v", err)
	}

	hc := apic.New(hub.URL, "hub-token")
	cl, err := hc.Clusters(ctx)
	if err != nil || len(cl) != 2 || !cl[0].Local || cl[1].Name != "lab-2" {
		t.Fatalf("clusters: %v %+v", err, cl)
	}

	// Remote: through the hub to lab-2.
	hc.Cluster = "lab-2"
	create(t, hc, "remote-1")
	v, err := hc.Version(ctx)
	if err != nil || v.Cluster != "lab-2" || v.ActiveSimulations != 1 {
		t.Fatalf("remote version: %v %+v", err, v)
	}
	direct := apic.New(agent.URL, "agent-token")
	if _, err := direct.Get(ctx, "sim-system", "remote-1"); err != nil {
		t.Fatalf("simulation not on the agent: %v", err)
	}

	// Local: the hub's own cluster.
	hc.Cluster = "central"
	create(t, hc, "local-1")
	if _, err := direct.Get(ctx, "sim-system", "local-1"); !isStatus(err, 404) {
		t.Fatalf("local simulation leaked to agent: %v", err)
	}

	// Unknown cluster.
	hc.Cluster = "nowhere"
	if _, err := hc.Catalog(ctx); !isStatus(err, 404) {
		t.Fatalf("want 404 for unknown cluster, got %v", err)
	}

	// Fleet view and emergency stop.
	hc.Cluster = ""
	fleet, err := hc.FleetSimulations(ctx, "testrun=t1")
	if err != nil || len(fleet) != 2 {
		t.Fatalf("fleet: %v %+v", err, fleet)
	}
	for _, f := range fleet {
		if f.Error != "" || len(f.Result) != 1 {
			t.Fatalf("fleet entry %+v", f)
		}
	}
	rec, err := hc.FleetRecoverAll(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, r := range rec {
		total += len(r.Result.Recovered)
	}
	if total != 2 {
		t.Fatalf("fleet recover-all recovered %d, want 2: %+v", total, rec)
	}

	// Probe marks the agent reachable.
	hubSrv.Hub.probeAll(ctx)
	var sc simv1.SimulationCluster
	_ = hubSrv.Client.Get(ctx, types.NamespacedName{Name: "lab-2"}, &sc)
	if !sc.Status.Reachable || sc.Status.AgentVersion != "test" {
		t.Fatalf("probe status %+v", sc.Status)
	}
	// A dead agent is reported unreachable, and fleet calls report the error.
	agent.Close()
	hubSrv.Hub.probeAll(ctx)
	_ = hubSrv.Client.Get(ctx, types.NamespacedName{Name: "lab-2"}, &sc)
	if sc.Status.Reachable {
		t.Fatal("closed agent still reachable")
	}
	fleet, _ = hc.FleetSimulations(ctx, "")
	if fleet[1].Error == "" {
		t.Fatalf("expected error for dead agent: %+v", fleet[1])
	}
}

func TestRegisterRefusesManualClusters(t *testing.T) {
	manual := &simv1.SimulationCluster{ObjectMeta: metav1.ObjectMeta{Name: "lab-3"}, Spec: simv1.SimulationClusterSpec{Endpoint: "https://x"}}
	_, hub := newServer("central", "hub-token", true, manual)
	defer hub.Close()
	c := apic.New(hub.URL, "reg-secret")
	err := c.Do(context.Background(), http.MethodPost, hub.URL+"/api/v1/clusters/register",
		apic.RegisterRequest{Name: "lab-3", Endpoint: "https://evil"}, nil)
	if !isStatus(err, 409) {
		t.Fatalf("want 409, got %v", err)
	}
	err = c.Do(context.Background(), http.MethodPost, hub.URL+"/api/v1/clusters/register",
		apic.RegisterRequest{Name: "central", Endpoint: "https://x"}, nil)
	if !isStatus(err, 400) {
		t.Fatalf("reserved name: want 400, got %v", err)
	}
}

func TestRegistrarHeartbeat(t *testing.T) {
	_, hub := newServer("central", "hub-token", true)
	defer hub.Close()
	r := &Registrar{Hub: apic.New(hub.URL, "reg-secret"), Interval: 10 * time.Millisecond, Log: logr.Discard(),
		Request: apic.RegisterRequest{Name: "lab-9", Endpoint: "https://lab-9:8443"}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_ = r.Start(ctx)
	cl, err := apic.New(hub.URL, "hub-token").Clusters(context.Background())
	if err != nil || len(cl) != 2 || cl[1].Name != "lab-9" {
		t.Fatalf("clusters after heartbeat: %v %+v", err, cl)
	}
}

func isStatus(err error, code int) bool {
	var ae *apic.APIError
	return errors.As(err, &ae) && ae.Status == code
}
