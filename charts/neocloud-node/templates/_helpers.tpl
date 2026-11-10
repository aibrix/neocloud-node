{{- define "neocloud.name" -}}
{{- printf "%s-neocloud-node" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- define "neocloud.clusterName" -}}
{{- printf "%s-%s" .Release.Namespace (include "neocloud.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
