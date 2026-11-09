{{- define "runaisim.labels" -}}
app.kubernetes.io/part-of: testenvs
testenv.onit.ai/name: {{ .Values.global.envName }}
{{- end }}

{{/* Child Application. dict: root, key, c (component values), source, ignoreDifferences, syncOptions */}}
{{- define "runaisim.app" -}}
{{- $root := .root -}}
{{- $c := .c -}}
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: {{ $root.Values.global.envName }}-{{ .key }}
  namespace: {{ $root.Values.global.argocdNamespace }}
  labels:
    {{- include "runaisim.labels" $root | nindent 4 }}
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
        {{- include "runaisim.labels" $root | nindent 8 }}
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

{{- define "runaisim.helmSource" -}}
{{- $c := .c -}}
{{- $h := dict "releaseName" .release "valuesObject" $c.values -}}
{{- if .skipCrds }}{{- $_ := set $h "skipCrds" true -}}{{- end -}}
{{- toYaml (dict "repoURL" $c.repoURL "chart" $c.chart "targetRevision" $c.version "helm" $h) -}}
{{- end }}
