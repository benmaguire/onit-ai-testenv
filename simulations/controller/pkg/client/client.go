// Package client is a Go client for the simulation API, for test runners.
//
//	c := client.New("https://sim-hub.example:8443", os.Getenv("SIM_TOKEN"))
//	c.Cluster = "lab-2"            // route through the hub to a remote agent
//	sim, _ := c.Create(ctx, client.CreateRequest{Spec: simv1.SimulationSpec{
//	    Type: "gpu-xid", Params: map[string]string{"xid": "79"},
//	    Target: simv1.Target{Graph: "mocker-agg", Component: "decode"},
//	    Duration: &metav1.Duration{Duration: time.Minute}}})
//	_, _ = c.WaitFor(ctx, sim.Namespace, sim.Name, simv1.PhaseRecovered)
package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	simv1 "github.com/benmaguire/onit-ai-testenv/simulations/controller/api/v1alpha1"
	"github.com/benmaguire/onit-ai-testenv/simulations/controller/pkg/catalog"
)

// Client talks to an agent, or to a hub (optionally routed to Cluster).
type Client struct {
	BaseURL string
	Token   string
	// Cluster routes calls through a hub to that cluster's agent. Empty
	// talks to the server itself.
	Cluster string
	HTTP    *http.Client
}

// New returns a client with a 30s timeout.
func New(baseURL, token string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// Insecure disables TLS verification (test labs with self-signed certs).
func (c *Client) Insecure() *Client {
	c.HTTP.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec
	return c
}

// APIError is a non-2xx response.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message) }

func (c *Client) path(p string) string {
	if c.Cluster != "" {
		return c.BaseURL + "/api/v1/clusters/" + url.PathEscape(c.Cluster) + p
	}
	return c.BaseURL + "/api/v1" + p
}

// Do performs a request against a full URL and decodes the JSON response.
func (c *Client) Do(ctx context.Context, method, u string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e ErrorResponse
		if json.Unmarshal(data, &e) != nil || e.Error == "" {
			e.Error = strings.TrimSpace(string(data))
		}
		return &APIError{Status: resp.StatusCode, Message: e.Error}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// Version of the server (or the routed agent).
func (c *Client) Version(ctx context.Context) (*VersionInfo, error) {
	var v VersionInfo
	u := c.BaseURL + "/version"
	if c.Cluster != "" {
		u = c.path("/version")
	}
	return &v, c.Do(ctx, http.MethodGet, u, nil, &v)
}

// Catalog lists available simulation types.
func (c *Client) Catalog(ctx context.Context) ([]catalog.Entry, error) {
	var out []catalog.Entry
	return out, c.Do(ctx, http.MethodGet, c.path("/catalog"), nil, &out)
}

// ListOptions filters List.
type ListOptions struct {
	Namespace     string
	LabelSelector string
	Phase         simv1.Phase
}

// List Simulations.
func (c *Client) List(ctx context.Context, o ListOptions) ([]simv1.Simulation, error) {
	q := url.Values{}
	if o.Namespace != "" {
		q.Set("namespace", o.Namespace)
	}
	if o.LabelSelector != "" {
		q.Set("labelSelector", o.LabelSelector)
	}
	if o.Phase != "" {
		q.Set("phase", string(o.Phase))
	}
	var out simv1.SimulationList
	u := c.path("/simulations")
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return out.Items, c.Do(ctx, http.MethodGet, u, nil, &out)
}

// Create (inject) a Simulation.
func (c *Client) Create(ctx context.Context, r CreateRequest) (*simv1.Simulation, error) {
	var out simv1.Simulation
	return &out, c.Do(ctx, http.MethodPost, c.path("/simulations"), r, &out)
}

// Get a Simulation.
func (c *Client) Get(ctx context.Context, namespace, name string) (*simv1.Simulation, error) {
	var out simv1.Simulation
	return &out, c.Do(ctx, http.MethodGet, c.path("/simulations/"+url.PathEscape(namespace)+"/"+url.PathEscape(name)), nil, &out)
}

// Recover requests recovery of a Simulation now.
func (c *Client) Recover(ctx context.Context, namespace, name string) (*simv1.Simulation, error) {
	var out simv1.Simulation
	return &out, c.Do(ctx, http.MethodPost, c.path("/simulations/"+url.PathEscape(namespace)+"/"+url.PathEscape(name)+"/recover"), nil, &out)
}

// Delete a Simulation (its fault is reverted first by the controller).
func (c *Client) Delete(ctx context.Context, namespace, name string) error {
	return c.Do(ctx, http.MethodDelete, c.path("/simulations/"+url.PathEscape(namespace)+"/"+url.PathEscape(name)), nil, nil)
}

// RecoverAll asks every active Simulation (optionally filtered) to recover.
func (c *Client) RecoverAll(ctx context.Context, namespace, labelSelector string) (*RecoverAllResponse, error) {
	q := url.Values{}
	if namespace != "" {
		q.Set("namespace", namespace)
	}
	if labelSelector != "" {
		q.Set("labelSelector", labelSelector)
	}
	var out RecoverAllResponse
	return &out, c.Do(ctx, http.MethodPost, c.path("/recover-all")+"?"+q.Encode(), nil, &out)
}

// Clusters registered with a hub.
func (c *Client) Clusters(ctx context.Context) ([]ClusterInfo, error) {
	var out []ClusterInfo
	return out, c.Do(ctx, http.MethodGet, c.BaseURL+"/api/v1/clusters", nil, &out)
}

// FleetSimulations lists Simulations on every cluster a hub manages.
func (c *Client) FleetSimulations(ctx context.Context, labelSelector string) ([]FleetResult[[]simv1.Simulation], error) {
	var out []FleetResult[[]simv1.Simulation]
	u := c.BaseURL + "/api/v1/fleet/simulations"
	if labelSelector != "" {
		u += "?labelSelector=" + url.QueryEscape(labelSelector)
	}
	return out, c.Do(ctx, http.MethodGet, u, nil, &out)
}

// FleetRecoverAll recovers every active Simulation on every cluster (emergency stop).
func (c *Client) FleetRecoverAll(ctx context.Context, labelSelector string) ([]FleetResult[RecoverAllResponse], error) {
	var out []FleetResult[RecoverAllResponse]
	u := c.BaseURL + "/api/v1/fleet/recover-all"
	if labelSelector != "" {
		u += "?labelSelector=" + url.QueryEscape(labelSelector)
	}
	return out, c.Do(ctx, http.MethodPost, u, nil, &out)
}

// WaitFor polls until the Simulation reaches phase (or Failed), or ctx ends.
func (c *Client) WaitFor(ctx context.Context, namespace, name string, phase simv1.Phase) (*simv1.Simulation, error) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		s, err := c.Get(ctx, namespace, name)
		if err != nil {
			return nil, err
		}
		if s.Status.Phase == phase {
			return s, nil
		}
		if s.Status.Phase == simv1.PhaseFailed {
			return s, fmt.Errorf("simulation %s/%s failed: %s", namespace, name, s.Status.Message)
		}
		if phase == simv1.PhaseActive && s.Status.Phase.Finished() {
			return s, fmt.Errorf("simulation %s/%s finished (%s) before reaching Active", namespace, name, s.Status.Phase)
		}
		select {
		case <-ctx.Done():
			return s, ctx.Err()
		case <-t.C:
		}
	}
}

// Report returns the simulation's timeline and baseline/fault/recovery/after measurements.
func (c *Client) Report(ctx context.Context, namespace, name string) (*Report, error) {
	var out Report
	return &out, c.Do(ctx, http.MethodGet, c.path("/simulations/"+url.PathEscape(namespace)+"/"+url.PathEscape(name)+"/report"), nil, &out)
}

// MetricsCSV returns the raw series and probe samples as CSV.
func (c *Client) MetricsCSV(ctx context.Context, namespace, name, step string) ([]byte, error) {
	u := c.path("/simulations/" + url.PathEscape(namespace) + "/" + url.PathEscape(name) + "/metrics?format=csv")
	if step != "" {
		u += "&step=" + url.QueryEscape(step)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(string(b))}
	}
	return b, err
}

// WaitObserved polls until the simulation finished and its observation is complete.
func (c *Client) WaitObserved(ctx context.Context, namespace, name string) (*simv1.Simulation, error) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		s, err := c.Get(ctx, namespace, name)
		if err != nil {
			return nil, err
		}
		if s.Status.Phase.Finished() && (s.Spec.Observe == nil || (s.Status.Observation != nil && s.Status.Observation.Complete)) {
			return s, nil
		}
		select {
		case <-ctx.Done():
			return s, ctx.Err()
		case <-t.C:
		}
	}
}

// Path returns the full URL of an /api/v1 path, routed to Cluster if set.
func (c *Client) Path(p string) string { return c.path(p) }
