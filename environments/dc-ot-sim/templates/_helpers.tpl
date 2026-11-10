{{- define "ot.labels" -}}
app.kubernetes.io/part-of: testenvs
testenv.onit.ai/name: {{ .Values.global.envName }}
{{- end }}
{{- define "ot.replicas" -}}{{ if eq .Values.state "down" }}0{{ else }}1{{ end }}{{- end }}
{{- define "ot.site" -}}
{{- $s := deepCopy .Values.siteConfig -}}
{{- $_ := set $s.site "region" ($s.site.region | default .Values.region) -}}
{{- toYaml $s -}}
{{- end }}
