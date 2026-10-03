{{/*
SPDX-License-Identifier: Apache-2.0
Copyright 2026 Gluesys Co., Ltd.
*/}}
{{- define "daos-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "daos-operator.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "daos-operator.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/name: {{ include "daos-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "daos-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "daos-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
control-plane: controller-manager
{{- end -}}

{{- define "daos-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "daos-operator.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "daos-operator.image" -}}
{{- printf "%s:%s" .Values.image.repository (include "daos-operator.tag" (list .Values.image.tag .Chart.AppVersion "image.tag")) -}}
{{- end -}}

{{/*
An image tag that YAML or --set read as a number (an all-digit commit hash such as 17519342) would
render as "%!s(float64=1.7519342e+07)", and a leading zero would already be lost. Refuse it.
Usage: include "daos-operator.tag" (list .Values.image.tag .Chart.AppVersion "image.tag")
*/}}
{{- define "daos-operator.tag" -}}
{{- $tag := index . 0 | default (index . 1) -}}
{{- if or (kindIs "float64" $tag) (kindIs "int64" $tag) (kindIs "int" $tag) -}}
{{- fail (printf "%s=%v was read as a number; quote it in values (tag: \"...\") or use --set-string" (index . 2) $tag) -}}
{{- end -}}
{{- $tag -}}
{{- end -}}
