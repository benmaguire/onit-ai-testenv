// Package kube contains small Kubernetes helpers: pod exec.
package kube

import (
	"bytes"
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// Executor runs a command in a container and returns its output.
type Executor interface {
	Exec(ctx context.Context, namespace, pod, container string, cmd []string) (stdout, stderr string, err error)
}

// PodExecutor implements Executor with the Kubernetes exec subresource.
type PodExecutor struct {
	Config    *rest.Config
	Clientset kubernetes.Interface
}

// NewPodExecutor builds a PodExecutor from a rest config.
func NewPodExecutor(cfg *rest.Config) (*PodExecutor, error) {
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &PodExecutor{Config: cfg, Clientset: cs}, nil
}

// Exec runs cmd in the container, preferring WebSocket and falling back to SPDY.
func (e *PodExecutor) Exec(ctx context.Context, namespace, pod, container string, cmd []string) (string, string, error) {
	req := e.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(namespace).Name(pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   cmd,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	ws, err := remotecommand.NewWebSocketExecutor(e.Config, "GET", req.URL().String())
	if err != nil {
		return "", "", err
	}
	spdy, err := remotecommand.NewSPDYExecutor(e.Config, "POST", req.URL())
	if err != nil {
		return "", "", err
	}
	exec, err := remotecommand.NewFallbackExecutor(ws, spdy, func(err error) bool { return true })
	if err != nil {
		return "", "", err
	}
	var out, errOut bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &out, Stderr: &errOut})
	if err != nil {
		return out.String(), errOut.String(), fmt.Errorf("exec %s/%s[%s] %v: %w: %s", namespace, pod, container, cmd, err, errOut.String())
	}
	return out.String(), errOut.String(), nil
}
