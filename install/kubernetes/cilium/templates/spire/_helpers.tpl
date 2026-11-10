{{/*
Use the conventional shared root only when independent stores need common trust.
Persistent single-server installations retain their existing CA unless overridden.
*/}}
{{- define "cilium.spire.upstreamSecret" -}}
{{- $server := .Values.authentication.mutual.spire.install.server -}}
{{- if $server.ca.upstreamSecret -}}
{{- $server.ca.upstreamSecret -}}
{{- else if and .Values.authentication.mutual.spire.ha.enabled (or (gt (int64 (include "cilium.spire.replicas" .)) 1) (not $server.dataStorage.enabled)) -}}
spire-upstream-ca
{{- end -}}
{{- end -}}

{{- define "cilium.spire.agentID" -}}
{{- printf "spiffe://%s/ns/%s/sa/%s" .Values.authentication.mutual.spire.trustDomain .Values.authentication.mutual.spire.install.namespace .Values.authentication.mutual.spire.install.agent.serviceAccount.name -}}
{{- end -}}

{{- define "cilium.spire.crdParentID" -}}
{{- if .Values.encryption.ztunnel.spire.parentID -}}
{{- .Values.encryption.ztunnel.spire.parentID -}}
{{- else -}}
{{- include "cilium.spire.agentID" . -}}
{{- end -}}
{{- end -}}

{{- define "cilium.spire.replicas" -}}
{{- $ha := .Values.authentication.mutual.spire.ha -}}
{{- if not (kindIs "map" $ha) -}}
  {{- fail "authentication.mutual.spire.ha must be an object." -}}
{{- end -}}
{{- if not (kindIs "bool" $ha.enabled) -}}
  {{- fail "authentication.mutual.spire.ha.enabled must be a boolean." -}}
{{- end -}}
{{- $replicas := 1 -}}
{{- if $ha.enabled -}}
  {{- $replicas = 2 -}}
{{- end -}}
{{- if hasKey $ha "replicas" -}}
  {{- if not $ha.enabled -}}
    {{- fail "authentication.mutual.spire.ha.replicas must be omitted when ha.enabled=false." -}}
  {{- end -}}
  {{- if or (kindIs "string" $ha.replicas) (not (regexMatch "^[1-9][0-9]*$" (toString $ha.replicas))) (gt (int64 $ha.replicas) 2147483647) -}}
    {{- fail "authentication.mutual.spire.ha.replicas must be an integer between 1 and 2147483647." -}}
  {{- end -}}
  {{- if not .Values.authentication.mutual.spire.install.enabled -}}
    {{- fail "authentication.mutual.spire.ha.replicas requires install.enabled=true; external SPIRE replicas cannot be configured by this chart." -}}
  {{- end -}}
  {{- $replicas = $ha.replicas -}}
{{- end -}}
{{- $replicas -}}
{{- end -}}

{{- define "cilium.spire.bootstrapEntries" -}}
{{- $trustDomain := .Values.authentication.mutual.spire.trustDomain -}}
{{- $agentID := include "cilium.spire.agentID" . -}}
{{- $entries := list
  (dict "name" (printf "cilium-spire-%s-%s-agent" (include "cilium.namespace" .) .Release.Name) "spec" (dict
    "spiffeID" $agentID "parentID" (printf "spiffe://%s/spire/server" $trustDomain)
    "selectors" (list
      (printf "k8s_psat:cluster:%s" .Values.cluster.name)
      (printf "k8s_psat:agent_ns:%s" .Values.authentication.mutual.spire.install.namespace)
      (printf "k8s_psat:agent_sa:%s" .Values.authentication.mutual.spire.install.agent.serviceAccount.name))))
  (dict "name" (printf "cilium-spire-%s-%s-ztunnel" (include "cilium.namespace" .) .Release.Name) "spec" (dict
    "spiffeID" (printf "spiffe://%s/ztunnel" $trustDomain) "parentID" $agentID
    "selectors" (list
      (printf "k8s:ns:%s" (include "cilium.namespace" .))
      (printf "k8s:sa:%s" .Values.serviceAccounts.ztunnel.name))))
-}}
{{- range $entry := $entries -}}
  {{- with $.Values.encryption.ztunnel.spire.className -}}
    {{- $_ := set $entry.spec "className" . -}}
  {{- end -}}
{{- end -}}
{{- $entries | mustToJson -}}
{{- end -}}
