{{- define "nvs.labels" -}}
app.kubernetes.io/part-of: testenvs
testenv.onit.ai/name: {{ .Values.global.envName }}
{{- end }}

{{/*
Common Application spec pieces.
Usage: include "nvs.app" (dict "root" $ "key" "mokka" "c" .Values.components.mokka "source" $source "extra" $extra)
*/}}
{{- define "nvs.app" -}}
{{- $root := .root -}}
{{- $c := .c -}}
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: {{ $root.Values.global.envName }}-{{ .key }}
  namespace: {{ $root.Values.global.argocdNamespace }}
  labels:
    {{- include "nvs.labels" $root | nindent 4 }}
    testenv.onit.ai/component: {{ .key }}
  annotations:
    argocd.argoproj.io/sync-wave: {{ $c.syncWave | quote }}
  finalizers:
    - resources-finalizer.argocd.argoproj.io
spec:
  project: {{ $root.Values.global.project }}
  source:
    {{- toYaml .source | nindent 4 }}
  destination:
    server: {{ $root.Values.global.destinationServer }}
    namespace: {{ $c.namespace }}
  {{- with .ignoreDifferences }}
  ignoreDifferences:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    managedNamespaceMetadata:
      labels:
        {{- include "nvs.labels" $root | nindent 8 }}
    syncOptions:
      - CreateNamespace=true
      - ServerSideApply=true
      {{- range .syncOptions }}
      - {{ . }}
      {{- end }}
    retry:
      limit: 20
      backoff:
        duration: 15s
        factor: 2
        maxDuration: 5m
{{- end }}

{{/*
Effective component for the current hardware mode. With hardware: real, every
key of the component's `real:` block replaces the simulated value (values are
replaced, not merged, so simulation-only settings do not leak into real mode).
Usage: $c := include "nvs.component" (dict "root" $ "c" .Values.components.x) | fromYaml
*/}}
{{- define "nvs.component" -}}
{{- $c := deepCopy .c -}}
{{- if and (eq (.root.Values.hardware | default "simulated") "real") (hasKey $c "real") -}}
{{- range $k, $v := $c.real }}{{ $_ := set $c $k $v }}{{ end -}}
{{- end -}}
{{- $_ := unset $c "real" -}}
{{- toYaml $c -}}
{{- end }}
