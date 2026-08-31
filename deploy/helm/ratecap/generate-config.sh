#!/usr/bin/env bash
set -euo pipefail

# Regenerates the embedded `config:`/`yaml: |` block in values.yaml from
# deploy/ratecap.yaml, the single source of truth for shipped defaults. Run
# this after any edit to deploy/ratecap.yaml and commit the resulting
# values.yaml diff — see the "Regenerating the embedded config" section in
# README.md. CI's helm-lint job runs this same script and fails the build if
# it produces a diff, so the two files can never silently drift again.

cd "$(dirname "$0")"

config_src="../../ratecap.yaml"
values_file="values.yaml"
begin_marker="# BEGIN GENERATED CONFIG (see generate-config.sh) — do not hand-edit between these markers"
end_marker="# END GENERATED CONFIG"

# awk extracts everything strictly outside the sentinel pair (before/after);
# the generated block itself is assembled separately below and spliced back
# in via printf, since this repo's awk (BSD/"one true" awk on macOS runners
# too) rejects embedded newlines in a -v string.
before=$(awk -v begin="$begin_marker" '$0 == begin { exit } { print }' "$values_file")
after=$(awk -v end="$end_marker" 'found { print } $0 == end { found = 1 }' "$values_file")

{
  printf '%s\n' "$before"
  printf '%s\n' "$begin_marker"
  printf 'config:\n  yaml: |\n'
  sed 's/^/    /' "$config_src"
  printf '%s\n' "$end_marker"
  printf '%s\n' "$after"
} > "$values_file.tmp"

mv "$values_file.tmp" "$values_file"
