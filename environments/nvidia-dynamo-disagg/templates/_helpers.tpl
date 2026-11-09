{{- define "r" -}}
{{- if eq .root.Values.state "down" }}0{{ else }}{{ .n }}{{ end -}}
{{- end }}

{{- define "worker" -}}
{{- $w := .w -}}
podTemplate:
  spec:
    containers:
      - name: main
        image: {{ .root.Values.image }}
        workingDir: /workspace
        command: [python3, -m, dynamo.mocker]
        args:
          - --model-path
          - {{ .root.Values.model | quote }}
          - --model-name
          - {{ .root.Values.model | quote }}
          - --speedup-ratio
          - {{ .root.Values.speedupRatio | quote }}
          - --disaggregation-mode
          - {{ .mode }}
          {{- range $w.extraArgs }}
          - {{ . | quote }}
          {{- end }}
        resources:
          requests: {{- toYaml $w.resources.requests | nindent 12 }}
          limits:
            {{- toYaml $w.resources.limits | nindent 12 }}
            {{- if gt (int $w.gpus) 0 }}
            nvidia.com/gpu: {{ $w.gpus | quote }}
            {{- end }}
{{- end }}
