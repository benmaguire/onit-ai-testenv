package kube

import (
	"context"
	"fmt"
	"io"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// EphemeralSpec is a short-lived debug container added to a target pod. It
// shares the pod's network namespace (and, with TargetContainer, the process
// namespace of that container), which is how network and process faults are
// applied without changing the workload.
type EphemeralSpec struct {
	Name            string
	Image           string
	Command         []string
	Capabilities    []corev1.Capability
	TargetContainer string
}

// EphemeralResult reports how the container ended (or that it is running).
type EphemeralResult struct {
	Running  bool
	ExitCode int32
	Logs     string
}

// EphemeralRunner adds an ephemeral container and waits for it.
type EphemeralRunner interface {
	// Run adds the container. With wait > 0 it waits up to wait for the
	// container to exit; with wait == 0 it returns once it is running.
	Run(ctx context.Context, namespace, pod string, spec EphemeralSpec, wait time.Duration) (EphemeralResult, error)
}

// PodEphemeralRunner implements EphemeralRunner with the pods/ephemeralcontainers subresource.
type PodEphemeralRunner struct {
	Clientset kubernetes.Interface
	// StartTimeout bounds image pull + start (default 3m).
	StartTimeout time.Duration
	Poll         time.Duration
}

// Run implements EphemeralRunner.
func (r *PodEphemeralRunner) Run(ctx context.Context, ns, name string, spec EphemeralSpec, wait time.Duration) (EphemeralResult, error) {
	pods := r.Clientset.CoreV1().Pods(ns)
	pod, err := pods.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return EphemeralResult{}, err
	}
	root := int64(0)
	nonRoot := false
	pod.Spec.EphemeralContainers = append(pod.Spec.EphemeralContainers, corev1.EphemeralContainer{
		TargetContainerName: spec.TargetContainer,
		EphemeralContainerCommon: corev1.EphemeralContainerCommon{
			Name:            spec.Name,
			Image:           spec.Image,
			Command:         spec.Command,
			ImagePullPolicy: corev1.PullIfNotPresent,
			SecurityContext: &corev1.SecurityContext{
				RunAsUser:    &root,
				RunAsNonRoot: &nonRoot,
				Capabilities: &corev1.Capabilities{Add: spec.Capabilities},
			},
		},
	})
	if _, err := pods.UpdateEphemeralContainers(ctx, name, pod, metav1.UpdateOptions{}); err != nil {
		return EphemeralResult{}, fmt.Errorf("add ephemeral container to %s/%s: %w", ns, name, err)
	}
	start := r.StartTimeout
	if start == 0 {
		start = 3 * time.Minute
	}
	poll := r.Poll
	if poll == 0 {
		poll = time.Second
	}
	deadline := time.Now().Add(start + wait)
	lastReason := ""
	for {
		p, err := pods.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return EphemeralResult{}, err
		}
		for _, st := range p.Status.EphemeralContainerStatuses {
			if st.Name != spec.Name {
				continue
			}
			switch {
			case st.State.Terminated != nil:
				res := EphemeralResult{ExitCode: st.State.Terminated.ExitCode, Logs: r.logs(ctx, ns, name, spec.Name)}
				if res.ExitCode != 0 {
					return res, fmt.Errorf("%s exited %d: %s", spec.Name, res.ExitCode, tail(res.Logs, 500))
				}
				return res, nil
			case st.State.Running != nil && wait == 0:
				return EphemeralResult{Running: true}, nil
			case st.State.Waiting != nil:
				lastReason = st.State.Waiting.Reason + ": " + st.State.Waiting.Message
			}
		}
		if time.Now().After(deadline) {
			return EphemeralResult{}, fmt.Errorf("ephemeral container %s in %s/%s did not finish in time (%s)", spec.Name, ns, name, lastReason)
		}
		select {
		case <-ctx.Done():
			return EphemeralResult{}, ctx.Err()
		case <-time.After(poll):
		}
	}
}

func (r *PodEphemeralRunner) logs(ctx context.Context, ns, pod, container string) string {
	limit := int64(16 << 10)
	rc, err := r.Clientset.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{Container: container, LimitBytes: &limit}).Stream(ctx)
	if err != nil {
		return ""
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return string(b)
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
