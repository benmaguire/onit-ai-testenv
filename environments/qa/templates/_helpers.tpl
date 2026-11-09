{{/* Common labels for every resource in this environment. */}}
{{- define "testenv.labels" -}}
app.kubernetes.io/part-of: testenvs
app.kubernetes.io/managed-by: argocd
testenv.onit.ai/name: {{ .Values.global.envName }}
{{- end }}

{{/*
Replica count that honours the environment state.
Usage: {{ include "testenv.replicas" (list . 3) }} -> 3 when up, 0 when down.
*/}}
{{- define "testenv.replicas" -}}
{{- $root := index . 0 -}}
{{- $n := index . 1 -}}
{{- if eq $root.Values.state "down" }}0{{ else }}{{ $n }}{{ end -}}
{{- end }}
