// Command simulator runs the test-environment simulation controller.
//
//	--mode=agent  controller + API for this cluster (default)
//	--mode=hub    the same, plus a registry of remote agents, routing
//	              (/api/v1/clusters/{name}/...) and fleet-wide calls
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	simv1 "github.com/benmaguire/onit-ai-testenv/simulations/nvidia-sim/api/v1alpha1"
	"github.com/benmaguire/onit-ai-testenv/simulations/nvidia-sim/internal/api"
	"github.com/benmaguire/onit-ai-testenv/simulations/nvidia-sim/internal/controller"
	"github.com/benmaguire/onit-ai-testenv/simulations/nvidia-sim/internal/inject"
	"github.com/benmaguire/onit-ai-testenv/simulations/nvidia-sim/internal/kube"
	apic "github.com/benmaguire/onit-ai-testenv/simulations/nvidia-sim/pkg/client"
)

var version = "dev"

func main() {
	var (
		mode            = flag.String("mode", "agent", "agent or hub")
		clusterName     = flag.String("cluster-name", envOr("SIM_CLUSTER_NAME", "local"), "name of this cluster in the hub registry")
		environment     = flag.String("environment", envOr("SIM_ENVIRONMENT", "nvidia-sim"), "test environment this cluster runs")
		apiAddr         = flag.String("api-addr", ":8443", "API listen address")
		tlsCert         = flag.String("tls-cert-file", "", "TLS certificate for the API (plain HTTP when empty)")
		tlsKey          = flag.String("tls-key-file", "", "TLS key for the API")
		tokenFile       = flag.String("api-token-file", envOr("SIM_API_TOKEN_FILE", ""), "file with the bearer token the API requires (auth disabled when empty)")
		regTokenFile    = flag.String("registration-token-file", "", "hub: file with the token agents use to self-register")
		probeAddr       = flag.String("health-probe-addr", ":8081", "controller health probe address")
		metricsAddr     = flag.String("metrics-addr", ":8080", "metrics address (0 disables)")
		leaderElect     = flag.Bool("leader-elect", false, "enable leader election")
		namespace       = flag.String("namespace", envOr("POD_NAMESPACE", "sim-system"), "namespace the simulator runs in (hub token secrets, default for new simulations)")
		mokkaNS         = flag.String("mokka-namespace", "nvidia-sim-mokka", "namespace of the nvml-mock DaemonSet")
		mokkaSel        = flag.String("mokka-selector", "app.kubernetes.io/name=nvml-mock", "label selector for nvml-mock pods")
		graph           = flag.String("default-graph", "mocker-agg", "default DynamoGraphDeployment for dynamo-* simulations")
		graphNS         = flag.String("default-graph-namespace", "nvidia-sim", "namespace of the default graph")
		hubURL          = flag.String("hub-url", "", "agent: hub to self-register with")
		hubTokenFile    = flag.String("hub-registration-token-file", "", "agent: file with the hub's registration token")
		advertiseURL    = flag.String("advertise-url", "", "agent: URL the hub should use to reach this agent")
		advertiseCAFile = flag.String("advertise-ca-file", "", "agent: CA bundle the hub should use to verify this agent")
		hubInsecure     = flag.Bool("hub-insecure-skip-tls-verify", false, "agent: skip TLS verification of the hub, and ask the hub to skip it for this agent")
	)
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("simulator")

	if err := run(log, runOpts{
		mode: *mode, clusterName: *clusterName, environment: *environment, apiAddr: *apiAddr,
		tlsCert: *tlsCert, tlsKey: *tlsKey, tokenFile: *tokenFile, regTokenFile: *regTokenFile,
		probeAddr: *probeAddr, metricsAddr: *metricsAddr, leaderElect: *leaderElect, namespace: *namespace,
		mokkaNS: *mokkaNS, mokkaSel: *mokkaSel, graph: *graph, graphNS: *graphNS,
		hubURL: *hubURL, hubTokenFile: *hubTokenFile, advertiseURL: *advertiseURL,
		advertiseCAFile: *advertiseCAFile, hubInsecure: *hubInsecure,
	}); err != nil {
		log.Error(err, "exiting")
		os.Exit(1)
	}
}

type runOpts struct {
	mode, clusterName, environment, apiAddr, tlsCert, tlsKey, tokenFile, regTokenFile string
	probeAddr, metricsAddr                                                            string
	leaderElect                                                                       bool
	namespace, mokkaNS, mokkaSel, graph, graphNS                                      string
	hubURL, hubTokenFile, advertiseURL, advertiseCAFile                               string
	hubInsecure                                                                       bool
}

func run(log interface {
	Info(string, ...any)
	Error(error, string, ...any)
}, o runOpts) error {
	if o.mode != "agent" && o.mode != "hub" {
		return fmt.Errorf("--mode must be agent or hub, got %q", o.mode)
	}
	token, err := readOptional(o.tokenFile)
	if err != nil {
		return err
	}
	if token == "" {
		log.Info("WARNING: API authentication is disabled (no --api-token-file)")
	}

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = simv1.AddToScheme(scheme)

	cfg := ctrl.GetConfigOrDie()
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: o.metricsAddr},
		HealthProbeBindAddress: o.probeAddr,
		LeaderElection:         o.leaderElect,
		LeaderElectionID:       "testenv-simulator." + simv1.GroupVersion.Group,
		// Read Secrets (hub agent tokens) directly instead of caching every
		// Secret in the cluster.
		Client: client.Options{Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.Secret{}}}},
	})
	if err != nil {
		return err
	}
	execr, err := kube.NewPodExecutor(cfg)
	if err != nil {
		return err
	}
	mokkaSelector, err := parseSelector(o.mokkaSel)
	if err != nil {
		return err
	}
	registry := inject.NewRegistry(inject.All()...)
	env := &inject.Env{
		Client: mgr.GetClient(), Exec: execr, Log: ctrl.Log.WithName("inject"),
		MokkaNamespace: o.mokkaNS, MokkaSelector: mokkaSelector,
		DefaultGraph: o.graph, DefaultGraphNamespace: o.graphNS, Now: time.Now,
	}
	if err := (&controller.SimulationReconciler{
		Client: mgr.GetClient(), Env: env, Registry: registry,
		Recorder: mgr.GetEventRecorderFor("testenv-simulator"),
	}).SetupWithManager(mgr); err != nil {
		return err
	}

	srv := &api.Server{
		Client: mgr.GetClient(), Registry: registry, ClusterName: o.clusterName, Environment: o.environment,
		Version: version, DefaultNamespace: o.namespace, Token: token, Log: ctrl.Log.WithName("api"),
	}
	if o.mode == "hub" {
		if srv.RegistrationToken, err = readOptional(o.regTokenFile); err != nil {
			return err
		}
		srv.Hub = &api.Hub{Client: mgr.GetClient(), Namespace: o.namespace}
		if err := mgr.Add(srv.Hub); err != nil {
			return err
		}
	}
	if o.hubURL != "" {
		if o.mode != "agent" {
			return errors.New("--hub-url is for agents")
		}
		if o.advertiseURL == "" {
			return errors.New("--advertise-url is required with --hub-url")
		}
		regToken, err := readOptional(o.hubTokenFile)
		if err != nil {
			return err
		}
		ca, err := readOptional(o.advertiseCAFile)
		if err != nil {
			return err
		}
		hc := apic.New(o.hubURL, regToken)
		if o.hubInsecure {
			hc.Insecure()
		}
		if err := mgr.Add(&api.Registrar{Hub: hc, Log: ctrl.Log.WithName("registrar"), Request: apic.RegisterRequest{
			Name: o.clusterName, Endpoint: o.advertiseURL, Token: token, Environment: o.environment,
			CABundle: ca, InsecureSkipTLSVerify: o.hubInsecure,
		}}); err != nil {
			return err
		}
	}
	if err := mgr.Add(&apiRunnable{addr: o.apiAddr, cert: o.tlsCert, key: o.tlsKey, handler: srv.Handler()}); err != nil {
		return err
	}
	_ = mgr.AddHealthzCheck("healthz", healthz.Ping)
	_ = mgr.AddReadyzCheck("readyz", healthz.Ping)

	log.Info("starting", "mode", o.mode, "cluster", o.clusterName, "version", version, "api", o.apiAddr)
	return mgr.Start(ctrl.SetupSignalHandler())
}

// apiRunnable serves the API on every replica (no leader election needed:
// it only reads and writes Simulation objects).
type apiRunnable struct {
	addr, cert, key string
	handler         http.Handler
}

func (a *apiRunnable) NeedLeaderElection() bool { return false }

func (a *apiRunnable) Start(ctx context.Context) error {
	s := &http.Server{Addr: a.addr, Handler: a.handler, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() {
		if a.cert != "" {
			errc <- s.ListenAndServeTLS(a.cert, a.key)
		} else {
			errc <- s.ListenAndServe()
		}
	}()
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.Shutdown(sctx)
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func readOptional(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func parseSelector(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, kv := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("selector %q: want key=value[,key=value]", s)
		}
		out[k] = v
	}
	return out, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
