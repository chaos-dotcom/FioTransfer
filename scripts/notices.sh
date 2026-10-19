#!/bin/bash
# Print the licenses of everything third-party in FioTransfer: the Go modules
# and standard library compiled into the app and the CLI, and the Inter font.
# Their licenses require these notices to ship with the app.
#
#   scripts/notices.sh > THIRD_PARTY_NOTICES.txt
set -euo pipefail
cd "$(dirname "$0")/.."

section() {
  printf '\n%s\n%s\n%s\n\n' "------------------------------------------------------------------------" "$1" "------------------------------------------------------------------------"
}

echo "FioTransfer includes the third-party software below. Each part is covered"
echo "by its own license, reproduced here."

section "Go standard library and runtime (https://go.dev)"
goroot="$(go env GOROOT)"
cat "$goroot/LICENSE" 2>/dev/null || cat "$goroot/../LICENSE" # Homebrew keeps it one level up

for m in $(go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}}@{{.Version}}{{end}}{{end}}' . ./cmd/fio | sort -u); do
  dir="$(go list -m -f '{{.Dir}}' "${m%@*}")"
  files="$(find "$dir" -maxdepth 1 -type f \( -iname 'licen[cs]e*' -o -iname 'copying*' \) | sort)"
  if [[ -z "$files" ]]; then
    echo "no license file in $m" >&2
    exit 1
  fi
  section "${m%@*} ${m#*@}"
  while IFS= read -r f; do
    [[ "$f" == "$(echo "$files" | head -1)" ]] || printf '\n[%s]\n\n' "$(basename "$f")"
    cat "$f"
  done <<<"$files"
done

section "Inter font (https://github.com/rsms/inter)"
cat frontend/dist/fonts/OFL.txt
