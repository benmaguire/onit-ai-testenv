{{- define "nvsim.labels" -}}
app.kubernetes.io/part-of: testenvs
testenv.onit.ai/name: {{ .Values.global.envName }}
{{- end }}

{{/*
Common Application spec pieces.
Usage: include "nvsim.app" (dict "root" $ "key" "mokka" "c" .Values.components.mokka "source" $source "extra" $extra)
*/}}
{{- define "nvsim.app" -}}
{{- $root := .root -}}
{{- $c := .c -}}
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: {{ $root.Values.global.envName }}-{{ .key }}
  namespace: {{ $root.Values.global.argocdNamespace }}
  labels:
    {{- include "nvsim.labels" $root | nindent 4 }}
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
        {{- include "nvsim.labels" $root | nindent 8 }}
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
