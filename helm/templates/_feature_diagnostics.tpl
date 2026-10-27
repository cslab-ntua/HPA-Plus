{{- define "hpaplus.featureDiagnosticsEnv" -}}
- name: HPAPLUS_FEATURE_DUMP_DIR
  value: /tmp/hpaplus-features
- name: HPAPLUS_FEATURE_DUMP_MAX_FILES
  value: {{ .Values.featureDiagnostics.maxFiles | quote }}
- name: HPAPLUS_FEATURE_DUMP_MAX_BYTES
  value: {{ .Values.featureDiagnostics.maxBytes | quote }}
- name: HPAPLUS_FEATURE_DUMP_MAX_AGE_SECONDS
  value: {{ .Values.featureDiagnostics.maxAgeSeconds | quote }}
{{- end -}}
