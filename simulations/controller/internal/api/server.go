// Package api serves the simulation HTTP API (agent) and, in hub mode, the
// cluster registry, proxying to remote agents and fleet-wide calls.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simv1 "github.com/benmaguire/onit-ai-testenv/simulations/controller/api/v1alpha1"
	"github.com/benmaguire/onit-ai-testenv/simulations/controller/internal/inject"
	"github.com/benmaguire/onit-ai-testenv/simulations/controller/internal/observe"
	apic "github.com/benmaguire/onit-ai-testenv/simulations/controller/pkg/client"
)

// maxBody bounds request bodies.
const maxBody = 1 << 20

// Server is the HTTP API.
type Server struct {
	Client           client.Client
	Registry         *inject.Registry
	ClusterName      string
	Environment      string
	Version          string
	DefaultNamespace string
	// Token required as "Authorization: Bearer <token>". Empty disables auth.
	Token string
	// RegistrationToken is additionally accepted on /api/v1/clusters/register.
	RegistrationToken string
	// Observer measures simulations (reports, exports).
	Observer *observe.Observer
	// Hub enables hub routes when non-nil.
	Hub *Hub
	Log logr.Logger
}

// Handler builds the HTTP handler.
func (s *Server) Handler() http.Handler {
	local := s.localMux()
	root := http.NewServeMux()
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	root.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	root.Handle("/", s.auth(local, s.Token))
	if s.Hub != nil {
		root.Handle("POST /api/v1/clusters/register", s.auth(http.HandlerFunc(s.register), s.Token, s.RegistrationToken))
		hub := http.NewServeMux()
		hub.HandleFunc("GET /api/v1/clusters", s.listClusters)
		hub.HandleFunc("DELETE /api/v1/clusters/{cluster}", s.deleteCluster)
		hub.HandleFunc("GET /api/v1/fleet/simulations", s.fleetList)
		hub.HandleFunc("POST /api/v1/fleet/recover-all", s.fleetRecoverAll)
		hub.HandleFunc("/api/v1/clusters/{cluster}/{rest...}", func(w http.ResponseWriter, r *http.Request) {
			s.route(w, r, local)
		})
		authed := s.auth(hub, s.Token)
		root.Handle("/api/v1/clusters", authed)
		root.Handle("/api/v1/clusters/", authed)
		root.Handle("/api/v1/fleet/", authed)
	}
	return s.logRequests(root)
}

func (s *Server) localMux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("GET /version", s.version)
	m.HandleFunc("GET /api/v1/catalog", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, s.Registry.Catalog()) })
	m.HandleFunc("GET /api/v1/simulations", s.list)
	m.HandleFunc("POST /api/v1/simulations", s.create)
	m.HandleFunc("GET /api/v1/simulations/{namespace}/{name}", s.get)
	m.HandleFunc("DELETE /api/v1/simulations/{namespace}/{name}", s.delete)
	m.HandleFunc("POST /api/v1/simulations/{namespace}/{name}/recover", s.recover)
	m.HandleFunc("POST /api/v1/recover-all", s.recoverAll)
	m.HandleFunc("GET /api/v1/simulations/{namespace}/{name}/report", s.report)
	m.HandleFunc("GET /api/v1/simulations/{namespace}/{name}/metrics", s.metricsExport)
	m.HandleFunc("GET /api/v1/profiles", s.profiles)
	return m
}

// auth accepts any of the non-empty tokens; with none configured it allows all.
func (s *Server) auth(next http.Handler, tokens ...string) http.Handler {
	var valid [][]byte
	for _, t := range tokens {
		if t != "" {
			valid = append(valid, []byte(t))
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(valid) > 0 {
			got := []byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
			ok := false
			for _, v := range valid {
				if subtle.ConstantTimeCompare(got, v) == 1 {
					ok = true
				}
			}
			if !ok {
				writeErr(w, http.StatusUnauthorized, errors.New("missing or invalid bearer token"))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(rw, r)
		if r.URL.Path != "/healthz" && r.URL.Path != "/readyz" {
			s.Log.V(1).Info("api", "method", r.Method, "path", r.URL.Path, "status", rw.status, "duration", time.Since(start).String())
		}
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) { w.status = code; w.ResponseWriter.WriteHeader(code) }

// --- local handlers ------------------------------------------------------------

func (s *Server) versionInfo(ctx context.Context) apic.VersionInfo {
	mode := "agent"
	if s.Hub != nil {
		mode = "hub"
	}
	v := apic.VersionInfo{Name: "testenv-simulator", Version: s.Version, Mode: mode, Cluster: s.ClusterName, Environment: s.Environment}
	var list simv1.SimulationList
	if err := s.Client.List(ctx, &list); err == nil {
		for _, sim := range list.Items {
			if !sim.Status.Phase.Finished() {
				v.ActiveSimulations++
			}
		}
	}
	return v
}

func (s *Server) version(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.versionInfo(r.Context()))
}

func (s *Server) listSims(ctx context.Context, namespace, selector, phase string) (*simv1.SimulationList, error) {
	opts := []client.ListOption{}
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	if selector != "" {
		sel, err := labels.Parse(selector)
		if err != nil {
			return nil, badRequest{fmt.Errorf("labelSelector: %w", err)}
		}
		opts = append(opts, client.MatchingLabelsSelector{Selector: sel})
	}
	var list simv1.SimulationList
	if err := s.Client.List(ctx, &list, opts...); err != nil {
		return nil, err
	}
	if phase != "" {
		items := list.Items[:0]
		for _, it := range list.Items {
			if string(it.Status.Phase) == phase {
				items = append(items, it)
			}
		}
		list.Items = items
	}
	return &list, nil
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	list, err := s.listSims(r.Context(), q.Get("namespace"), q.Get("labelSelector"), q.Get("phase"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var req apic.CreateRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}
	inj, ok := s.Registry.Get(req.Spec.Type)
	if !ok {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("unknown simulation type %q (GET /api/v1/catalog)", req.Spec.Type))
		return
	}
	ns := req.Namespace
	if ns == "" {
		ns = s.DefaultNamespace
	}
	sim := &simv1.Simulation{
		ObjectMeta: metav1.ObjectMeta{Name: req.Name, Namespace: ns, Labels: req.Labels},
		Spec:       req.Spec,
	}
	if sim.Name == "" {
		sim.GenerateName = req.Spec.Type + "-"
	}
	// Validate early so callers get a 400 rather than a Failed simulation.
	if err := inj.Validate(sim); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.observer().ValidateSpec(sim); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.Client.Create(r.Context(), sim); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, sim)
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	sim := &simv1.Simulation{}
	key := types.NamespacedName{Namespace: r.PathValue("namespace"), Name: r.PathValue("name")}
	if err := s.Client.Get(r.Context(), key, sim); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, sim)
}

func (s *Server) delete(w http.ResponseWriter, r *http.Request) {
	sim := &simv1.Simulation{ObjectMeta: metav1.ObjectMeta{Namespace: r.PathValue("namespace"), Name: r.PathValue("name")}}
	if err := s.Client.Delete(r.Context(), sim); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) setRecover(ctx context.Context, key types.NamespacedName) (*simv1.Simulation, error) {
	sim := &simv1.Simulation{}
	if err := s.Client.Get(ctx, key, sim); err != nil {
		return nil, err
	}
	if sim.Spec.Recover {
		return sim, nil
	}
	patch := client.MergeFrom(sim.DeepCopy())
	sim.Spec.Recover = true
	return sim, s.Client.Patch(ctx, sim, patch)
}

func (s *Server) recover(w http.ResponseWriter, r *http.Request) {
	sim, err := s.setRecover(r.Context(), types.NamespacedName{Namespace: r.PathValue("namespace"), Name: r.PathValue("name")})
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusAccepted, sim)
}

func (s *Server) recoverAllLocal(ctx context.Context, namespace, selector string) (*apic.RecoverAllResponse, error) {
	list, err := s.listSims(ctx, namespace, selector, "")
	if err != nil {
		return nil, err
	}
	out := &apic.RecoverAllResponse{Recovered: []string{}}
	var errs []error
	for _, it := range list.Items {
		if it.Status.Phase.Finished() || it.Spec.Recover {
			continue
		}
		if _, err := s.setRecover(ctx, client.ObjectKeyFromObject(&it)); err != nil {
			errs = append(errs, err)
			continue
		}
		out.Recovered = append(out.Recovered, it.Namespace+"/"+it.Name)
	}
	return out, errors.Join(errs...)
}

func (s *Server) recoverAll(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	out, err := s.recoverAllLocal(r.Context(), q.Get("namespace"), q.Get("labelSelector"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusAccepted, out)
}

// --- helpers ---------------------------------------------------------------------

type badRequest struct{ error }

func statusFor(err error) int {
	var br badRequest
	switch {
	case errors.As(err, &br):
		return http.StatusBadRequest
	case apierrors.IsNotFound(err):
		return http.StatusNotFound
	case apierrors.IsAlreadyExists(err), apierrors.IsConflict(err):
		return http.StatusConflict
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return http.StatusBadRequest
	case apierrors.IsForbidden(err):
		return http.StatusForbidden
	}
	return http.StatusInternalServerError
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, apic.ErrorResponse{Error: err.Error()})
}
