{{/* Stable chart identity and selector labels. */}}
{{- define "hoorific.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- define "hoorific.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "hoorific.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- define "hoorific.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- define "hoorific.labels" -}}
helm.sh/chart: {{ include "hoorific.chart" . }}
app.kubernetes.io/name: {{ include "hoorific.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}
{{- define "hoorific.selectorLabels" -}}
app.kubernetes.io/name: {{ include "hoorific.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
{{- define "hoorific.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "hoorific.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}
{{- define "hoorific.image" -}}
{{- if .Values.image.digest -}}
{{ printf "%s@%s" .Values.image.repository .Values.image.digest }}
{{- else -}}
{{ printf "%s:%s" .Values.image.repository .Values.image.tag }}
{{- end -}}
{{- end -}}
{{/* Build JSON from typed dictionaries; credentials are referenced by file path only. */}}
{{- define "hoorific.config" -}}
{{- $postgresSecret := required "Values.existingSecrets.postgres.secretName is required" .Values.existingSecrets.postgres.secretName -}}
{{- if not .Values.existingSecrets.postgres.key }}{{ fail "Values.existingSecrets.postgres.key is required" }}{{ end -}}
{{- $encryptionSecret := required "Values.existingSecrets.encryption.secretName is required" .Values.existingSecrets.encryption.secretName -}}
{{- if not .Values.existingSecrets.encryption.key }}{{ fail "Values.existingSecrets.encryption.key is required" }}{{ end -}}
{{- if ne .Values.config.mode "cluster" }}{{ fail "Values.config.mode must be cluster when using external PostgreSQL" }}{{ end -}}
{{- $dsnFile := required "Values.config.storage.postgres.dsnFile is required" .Values.config.storage.postgres.dsnFile -}}
{{- $keyFile := required "Values.config.encryption.keyFile is required" .Values.config.encryption.keyFile -}}
{{- $dataDir := required "Values.config.dataDir is required" .Values.config.dataDir -}}
{{- $inferenceListener := required "Values.config.listeners.inference is required" .Values.config.listeners.inference -}}
{{- $managementListener := required "Values.config.listeners.management is required" .Values.config.listeners.management -}}
{{- $redisEnabled := .Values.config.coordination.redis.enabled -}}
{{- $oidcEnabled := .Values.config.oidc.enabled -}}
{{- if and $redisEnabled (not .Values.existingSecrets.redis.secretName) }}{{ fail "Values.existingSecrets.redis.secretName is required when config.coordination.redis.enabled" }}{{ end -}}
{{- if and $redisEnabled (not .Values.config.coordination.redis.urlFile) }}{{ fail "Values.config.coordination.redis.urlFile is required when Redis is enabled" }}{{ end -}}
{{- if $oidcEnabled }}
{{- if not .Values.existingSecrets.oidc.secretName }}{{ fail "Values.existingSecrets.oidc.secretName is required when config.oidc.enabled" }}{{ end -}}
{{- if not .Values.config.oidc.issuer }}{{ fail "Values.config.oidc.issuer is required when config.oidc.enabled" }}{{ end -}}
{{- if not .Values.config.oidc.clientId }}{{ fail "Values.config.oidc.clientId is required when config.oidc.enabled" }}{{ end -}}
{{- if not .Values.config.oidc.clientSecretFile }}{{ fail "Values.config.oidc.clientSecretFile is required when config.oidc.enabled" }}{{ end -}}
{{- end -}}
{{- $redisURLFile := "" -}}
{{- if $redisEnabled }}{{- $redisURLFile = .Values.config.coordination.redis.urlFile -}}{{- end -}}
{{- $oidcIssuer := "" -}}
{{- $oidcClientID := "" -}}
{{- $oidcSecretFile := "" -}}
{{- if $oidcEnabled }}
{{- $oidcIssuer = .Values.config.oidc.issuer -}}
{{- $oidcClientID = .Values.config.oidc.clientId -}}
{{- $oidcSecretFile = .Values.config.oidc.clientSecretFile -}}
{{- end -}}
{{- $redis := dict "url_file" $redisURLFile -}}
{{- $oidc := dict "issuer" $oidcIssuer "client_id" $oidcClientID "client_secret_file" $oidcSecretFile -}}
{{- $storage := dict "postgres" (dict "dsn_file" $dsnFile) -}}
{{- $coordination := dict "redis" $redis -}}
{{- $encryption := dict "key_file" $keyFile -}}
{{- $listeners := dict "inference" $inferenceListener "management" $managementListener -}}
{{- $cfg := dict "schema_version" .Values.config.schemaVersion "mode" .Values.config.mode "data_dir" $dataDir "listeners" $listeners "storage" $storage "coordination" $coordination "encryption" $encryption "oidc" $oidc "public_urls" .Values.config.publicUrls "subscription_connectors" (dict "enabled" .Values.config.subscriptionConnectors.enabled) -}}
{{- toJson $cfg -}}
{{- end -}}
