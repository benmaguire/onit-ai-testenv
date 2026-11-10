{{- define "r" -}}
{{- if eq .root.Values.state "down" }}0{{ else }}{{ .n }}{{ end -}}
{{- end }}

{{- define "worker" -}}
{{- $w := .w -}}
{{- $real := eq (.root.Values.hardware | default "simulated") "real" -}}
podTemplate:
  spec:
    containers:
      - name: main
        {{- if $real }}
        # Real NVIDIA GPU: vLLM with NIXL KV transfer between prefill and decode.
        image: {{ .root.Values.real.image }}
        workingDir: /workspace
        command: [python3, -m, dynamo.vllm]
        args:
          - --model
          - {{ .root.Values.model | quote }}
          - --disaggregation-mode
          - {{ .mode }}
          - --kv-transfer-config
          - {{ printf "{\"kv_connector\":\"NixlConnector\",\"kv_role\":\"%s\"}" (ternary "kv_producer" "kv_consumer" (eq .mode "prefill")) | squote }}
          {{- range .root.Values.real.extraArgs }}
          - {{ . | quote }}
          {{- end }}
        {{- with .root.Values.real.hfTokenSecret }}
        envFrom:
          - secretRef:
              name: {{ . }}
        {{- end }}
        resources:
          requests: {{- toYaml .root.Values.real.resources.requests | nindent 12 }}
          limits:
            {{- toYaml .root.Values.real.resources.limits | nindent 12 }}
            nvidia.com/gpu: {{ max 1 (int $w.gpus) | quote }}
        {{- else }}
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
{{- end }}
