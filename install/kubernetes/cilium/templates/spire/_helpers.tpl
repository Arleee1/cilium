{{/*
Encode a map for pflag.StringToString, including commas, quotes and newlines.
*/}}
{{- define "cilium.spire.registrationMap" -}}
{{- $entries := list -}}
{{- range $key, $value := . -}}
{{- $entries = append $entries (printf "\"%s\"" (replace "\"" "\"\"" (printf "%s=%v" $key $value))) -}}
{{- end -}}
{{- join "," $entries -}}
{{- end -}}

{{/*
CRD-managed servers have fresh datastores; the client-facing Service keeps its
original name in both registration modes.
*/}}
{{- define "cilium.spire.serverName" -}}
{{- if .Values.encryption.ztunnel.spire.useCRD -}}
spire-server-crd
{{- else -}}
spire-server
{{- end -}}
{{- end -}}

{{- define "cilium.spire.bundleName" -}}
{{- if .Values.encryption.ztunnel.spire.useCRD -}}
spire-server-crd-bundle
{{- else -}}
spire-bundle
{{- end -}}
{{- end -}}
