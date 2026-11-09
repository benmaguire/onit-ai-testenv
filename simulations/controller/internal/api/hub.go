package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simv1 "github.com/benmaguire/onit-ai-testenv/simulations/controller/api/v1alpha1"
	apic "github.com/benmaguire/onit-ai-testenv/simulations/controller/pkg/client"
)

const labelManagedBy = "sim.testenv.onit.ai/registered"

var clusterNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,51}[a-z0-9])?$`)

// Hub manages remote agents registered as SimulationClusters.
type Hub struct {
	Client client.Client
	// Namespace where agent token Secrets are stored.
	Namespace     string
	ProbeInterval time.Duration
	Timeout       time.Duration
}

// agent returns an API client for a registered cluster.
func (h *Hub) agent(ctx context.Context, name string) (*apic.Client, *simv1.SimulationCluster, error) {
	sc := &simv1.SimulationCluster{}
	if err := h.Client.Get(ctx, types.NamespacedName{Name: name}, sc); err != nil {
		return nil, nil, err
	}
	token := ""
	if ref := sc.Spec.TokenSecretRef; ref != nil {
		var sec corev1.Secret
		if err := h.Client.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, &sec); err != nil {
			return nil, sc, fmt.Errorf("token secret %s/%s: %w", ref.Namespace, ref.Name, err)
		}
		key := ref.Key
		if key == "" {
			key = "token"
		}
		token = strings.TrimSpace(string(sec.Data[key]))
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: sc.Spec.InsecureSkipTLSVerify} //nolint:gosec // opt-in for labs
	if sc.Spec.CABundle != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(sc.Spec.CABundle)) {
			return nil, sc, errors.New("caBundle contains no certificates")
		}
		tlsCfg.RootCAs = pool
	}
	timeout := h.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	c := apic.New(sc.Spec.Endpoint, token)
	c.HTTP = &http.Client{Timeout: timeout, Transport: &http.Transport{TLSClientConfig: tlsCfg, Proxy: http.ProxyFromEnvironment}}
	return c, sc, nil
}

// --- registry handlers -----------------------------------------------------------

func (s *Server) listClusters(w http.ResponseWriter, r *http.Request) {
	out := []apic.ClusterInfo{{
		Name: s.ClusterName, Endpoint: "local", Environment: s.Environment, Local: true, Reachable: true,
		AgentVersion: s.Version, ActiveSimulations: int32(s.versionInfo(r.Context()).ActiveSimulations),
	}}
	var list simv1.SimulationClusterList
	if err := s.Client.List(r.Context(), &list); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	for _, sc := range list.Items {
		ci := apic.ClusterInfo{
			Name: sc.Name, Endpoint: sc.Spec.Endpoint, Environment: sc.Spec.Environment,
			Reachable: sc.Status.Reachable, AgentVersion: sc.Status.AgentVersion,
			ActiveSimulations: sc.Status.ActiveSimulations, Message: sc.Status.Message,
		}
		if sc.Status.LastSeenTime != nil {
			t := sc.Status.LastSeenTime.Time
			ci.LastSeen = &t
		}
		out = append(out, ci)
	}
	sort.Slice(out[1:], func(a, b int) bool { return out[a+1].Name < out[b+1].Name })
	writeJSON(w, http.StatusOK, out)
}

// register upserts a SimulationCluster (and its token Secret) for an agent.
func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var req apic.RegisterRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if !clusterNameRE.MatchString(req.Name) || req.Name == s.ClusterName {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid or reserved cluster name %q", req.Name))
		return
	}
	if u, err := url.Parse(req.Endpoint); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("endpoint %q must be an http(s) URL", req.Endpoint))
		return
	}
	ctx := r.Context()
	h := s.Hub
	sc := &simv1.SimulationCluster{ObjectMeta: metav1.ObjectMeta{Name: req.Name}}
	err := s.Client.Get(ctx, types.NamespacedName{Name: req.Name}, sc)
	if err != nil && !apierrors.IsNotFound(err) {
		writeErr(w, statusFor(err), err)
		return
	}
	exists := err == nil
	if exists && sc.Labels[labelManagedBy] != "true" {
		writeErr(w, http.StatusConflict, fmt.Errorf("cluster %q is managed manually; refusing to overwrite", req.Name))
		return
	}
	if req.Token != "" {
		sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "simcluster-" + req.Name, Namespace: h.Namespace}}
		if err := upsertSecret(ctx, s.Client, sec, req.Token); err != nil {
			writeErr(w, statusFor(err), err)
			return
		}
	}
	spec := simv1.SimulationClusterSpec{
		Endpoint: req.Endpoint, Environment: req.Environment, CABundle: req.CABundle,
		InsecureSkipTLSVerify: req.InsecureSkipTLSVerify,
	}
	if req.Token != "" {
		spec.TokenSecretRef = &simv1.SecretKeyRef{Namespace: h.Namespace, Name: "simcluster-" + req.Name, Key: "token"}
	}
	sc.Labels = map[string]string{labelManagedBy: "true"}
	sc.Spec = spec
	if exists {
		err = s.Client.Update(ctx, sc)
	} else {
		err = s.Client.Create(ctx, sc)
	}
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	now := metav1.Now()
	sc.Status.LastSeenTime = &now
	_ = s.Client.Status().Update(ctx, sc)
	writeJSON(w, http.StatusOK, sc)
}

func upsertSecret(ctx context.Context, c client.Client, sec *corev1.Secret, token string) error {
	err := c.Get(ctx, client.ObjectKeyFromObject(sec), sec)
	sec.Labels = map[string]string{labelManagedBy: "true"}
	sec.Data = map[string][]byte{"token": []byte(token)}
	if apierrors.IsNotFound(err) {
		return c.Create(ctx, sec)
	}
	if err != nil {
		return err
	}
	return c.Update(ctx, sec)
}

func (s *Server) deleteCluster(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("cluster")
	sc := &simv1.SimulationCluster{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := s.Client.Delete(r.Context(), sc); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "simcluster-" + name, Namespace: s.Hub.Namespace}}
	_ = client.IgnoreNotFound(s.Client.Delete(r.Context(), sec))
	w.WriteHeader(http.StatusNoContent)
}

// route serves /api/v1/clusters/{cluster}/{rest...}: locally for the hub's own
// cluster, otherwise by forwarding to the agent.
func (s *Server) route(w http.ResponseWriter, r *http.Request, local http.Handler) {
	cluster, rest := r.PathValue("cluster"), r.PathValue("rest")
	target := "/api/v1/" + rest
	if rest == "version" {
		target = "/version"
	}
	if cluster == s.ClusterName {
		r2 := r.Clone(r.Context())
		r2.URL.Path = target
		r2.URL.RawPath = ""
		local.ServeHTTP(w, r2)
		return
	}
	ac, _, err := s.Hub.agent(r.Context(), cluster)
	if err != nil {
		code := statusFor(err)
		if apierrors.IsNotFound(err) {
			err = fmt.Errorf("cluster %q is not registered (GET /api/v1/clusters)", cluster)
		}
		writeErr(w, code, err)
		return
	}
	u := strings.TrimRight(ac.BaseURL, "/") + target
	if r.URL.RawQuery != "" {
		u += "?" + r.URL.RawQuery
	}
	out, err := http.NewRequestWithContext(r.Context(), r.Method, u, http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out.Header.Set("Content-Type", r.Header.Get("Content-Type"))
	if ac.Token != "" {
		out.Header.Set("Authorization", "Bearer "+ac.Token)
	}
	resp, err := ac.HTTP.Do(out)
	if err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Errorf("agent %s: %w", cluster, err))
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// --- fleet-wide ----------------------------------------------------------------

// forEachCluster runs fn for the local cluster and every registered agent in parallel.
func forEachCluster[T any](s *Server, ctx context.Context, local func() (T, error), remote func(*apic.Client) (T, error)) []apic.FleetResult[T] {
	var list simv1.SimulationClusterList
	listErr := s.Client.List(ctx, &list)
	results := make([]apic.FleetResult[T], len(list.Items)+1)
	var wg sync.WaitGroup
	run := func(i int, name string, f func() (T, error)) {
		defer wg.Done()
		res, err := f()
		results[i] = apic.FleetResult[T]{Cluster: name, Result: res}
		if err != nil {
			results[i].Error = err.Error()
		}
	}
	wg.Add(1)
	go run(0, s.ClusterName, local)
	for i, sc := range list.Items {
		wg.Add(1)
		go run(i+1, sc.Name, func() (T, error) {
			var zero T
			ac, _, err := s.Hub.agent(ctx, sc.Name)
			if err != nil {
				return zero, err
			}
			return remote(ac)
		})
	}
	wg.Wait()
	if listErr != nil {
		results = append(results, apic.FleetResult[T]{Cluster: "*", Error: "listing clusters: " + listErr.Error()})
	}
	return results
}

func (s *Server) fleetList(w http.ResponseWriter, r *http.Request) {
	sel := r.URL.Query().Get("labelSelector")
	ctx := r.Context()
	res := forEachCluster(s, ctx,
		func() ([]simv1.Simulation, error) {
			l, err := s.listSims(ctx, "", sel, "")
			if err != nil {
				return nil, err
			}
			return l.Items, nil
		},
		func(ac *apic.Client) ([]simv1.Simulation, error) {
			return ac.List(ctx, apic.ListOptions{LabelSelector: sel})
		})
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) fleetRecoverAll(w http.ResponseWriter, r *http.Request) {
	sel := r.URL.Query().Get("labelSelector")
	ctx := r.Context()
	res := forEachCluster(s, ctx,
		func() (apic.RecoverAllResponse, error) {
			out, err := s.recoverAllLocal(ctx, "", sel)
			if out == nil {
				out = &apic.RecoverAllResponse{}
			}
			return *out, err
		},
		func(ac *apic.Client) (apic.RecoverAllResponse, error) {
			out, err := ac.RecoverAll(ctx, "", sel)
			if out == nil {
				out = &apic.RecoverAllResponse{}
			}
			return *out, err
		})
	writeJSON(w, http.StatusAccepted, res)
}

// --- health probing --------------------------------------------------------------

// Start probes every registered agent periodically (manager.Runnable).
func (h *Hub) Start(ctx context.Context) error {
	interval := h.ProbeInterval
	if interval == 0 {
		interval = 30 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		h.probeAll(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// NeedLeaderElection: probing writes status, so only the leader probes.
func (h *Hub) NeedLeaderElection() bool { return true }

func (h *Hub) probeAll(ctx context.Context) {
	var list simv1.SimulationClusterList
	if err := h.Client.List(ctx, &list); err != nil {
		return
	}
	for i := range list.Items {
		h.probe(ctx, &list.Items[i])
	}
}

func (h *Hub) probe(ctx context.Context, sc *simv1.SimulationCluster) {
	now := metav1.Now()
	sc.Status.LastProbeTime = &now
	ac, _, err := h.agent(ctx, sc.Name)
	if err == nil {
		var v *apic.VersionInfo
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		v, err = ac.Version(pctx)
		cancel()
		if err == nil {
			sc.Status.Reachable = true
			sc.Status.LastSeenTime = &now
			sc.Status.AgentVersion = v.Version
			sc.Status.ActiveSimulations = int32(v.ActiveSimulations)
			sc.Status.Message = ""
		}
	}
	if err != nil {
		sc.Status.Reachable = false
		sc.Status.Message = err.Error()
	}
	_ = h.Client.Status().Update(ctx, sc)
}
