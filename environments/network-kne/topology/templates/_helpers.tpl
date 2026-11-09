{{- define "kne.routers" -}}
{{- list
  (dict "name" "r1" "peer" "r2" "host" "host1" "loopback" "1.1.1.1" "peerLoopback" "2.2.2.2" "p2p" "192.168.12.1" "lan" "192.168.1.1" "lanNet" "192.168.1.0")
  (dict "name" "r2" "peer" "r1" "host" "host2" "loopback" "2.2.2.2" "peerLoopback" "1.1.1.1" "p2p" "192.168.12.2" "lan" "192.168.2.1" "lanNet" "192.168.2.0")
  | toJson -}}
{{- end }}

{{- define "kne.files" -}}
topology.pb.txt: |-
  {{- tpl (.Files.Get "files/topology.pb.txt") . | nindent 2 }}
{{- range $r := include "kne.routers" . | fromJsonArray }}
{{ $r.name }}.config: |-
  {{- tpl ($.Files.Get "files/router.config") (merge (dict "Values" $.Values "Template" $.Template) $r) | nindent 2 }}
{{- end }}
{{- end }}
