{{- define "w.replicas" -}}
{{- if eq .root.Values.state "down" }}0{{ else }}{{ .n }}{{ end -}}
{{- end }}

{{/* Pod template for a workload running on the KWOK virtual GPU nodes. */}}
{{- define "w.pod" -}}
{{- $w := .w -}}
metadata:
  labels:
    app: {{ $w.name }}
    kai.scheduler/queue: {{ $w.queue }}
    testenv.onit.ai/name: {{ .root.Values.envName }}
  annotations:
    {{- with $w.gpuFraction }}
    gpu-fraction: {{ . | quote }}
    {{- end }}
    {{- with $w.utilization }}
    run.ai/simulated-gpu-utilization: {{ . | quote }}
    {{- end }}
spec:
  schedulerName: kai-scheduler
  {{- with $w.priorityClassName }}
  priorityClassName: {{ . }}
  {{- end }}
  {{- if eq $w.kind "Job" }}
  restartPolicy: Never
  {{- end }}
  nodeSelector:
    type: kwok
  tolerations:
    - key: kwok.x-k8s.io/node
      operator: Equal
      value: fake
      effect: NoSchedule
  containers:
    - name: main
      image: {{ .root.Values.image }}
      resources:
        requests:
          cpu: 100m
          memory: 128Mi
        {{- with $w.gpus }}
        limits:
          nvidia.com/gpu: {{ . | quote }}
        {{- end }}
{{- end }}
