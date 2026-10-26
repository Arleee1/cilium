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

{{/*
Name of the Secret holding the shared SPIRE upstream root. Multiple
CRD-managed servers need a common root so every server issues SVIDs that
chain to the same trust bundle.
*/}}
{{- define "cilium.spire.upstreamSecret" -}}
{{- $server := .Values.authentication.mutual.spire.install.server -}}
{{- if $server.ca.upstreamSecret -}}
{{- $server.ca.upstreamSecret -}}
{{- else if and .Values.encryption.ztunnel.spire.useCRD (gt (int64 $server.replicas) 1) -}}
spire-upstream-ca
{{- end -}}
{{- end -}}
