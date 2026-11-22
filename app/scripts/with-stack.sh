#!/usr/bin/env bash
# Read a launch request from stdin and run a command with STACK_URL set.
set -uo pipefail

if (($# == 0)); then
  echo 'Usage: with-stack.sh command [args...] < request.json' >&2
  exit 2
fi
base=${STACK_LAUNCHER_URL:-http://127.0.0.1:8080}
base=${base%/}
request_timeout=${STACK_REQUEST_TIMEOUT:-30}
poll_timeout=${STACK_POLL_TIMEOUT:-300}
poll_interval=${STACK_POLL_INTERVAL:-2}
cleanup_timeout=${STACK_CLEANUP_TIMEOUT:-300}
cleanup_interval=${STACK_CLEANUP_INTERVAL:-2}
for value in "$request_timeout" "$poll_timeout" "$poll_interval" "$cleanup_timeout" "$cleanup_interval"; do
  if [[ ! $value =~ ^[1-9][0-9]*$ ]]; then
    echo 'Timeouts and intervals must be positive integer seconds' >&2
    exit 2
  fi
done
if [[ ! $base =~ ^https?://[^/@?#[:space:]]+$ ]]; then
  echo 'STACK_LAUNCHER_URL must be an HTTP(S) origin' >&2
  exit 2
fi
for tool in curl jq; do command -v "$tool" >/dev/null || { echo "Missing $tool" >&2; exit 2; }; done
curl_options=(--disable --globoff --silent --show-error)
tmp=$(mktemp -d) || exit 1
id=''
child=''
post_reported=false
id_pattern='^[a-z0-9]([a-z0-9-]*[a-z0-9])?$'

accepted_location() {
  awk -v pattern="$id_pattern" '
    { sub(/\r$/, "") }
    /^HTTP\// { accepted = ($2 == 202); id = "" }
    accepted && tolower($1) == "location:" && $2 ~ /^\/stacks\// && NF == 2 {
      candidate = substr($2, 9)
      if (length(candidate) <= 63 && candidate ~ pattern) id = candidate
    }
    END { if (id != "") print id }
  ' "$tmp/headers" 2>/dev/null
}

http_failure() {
  local operation=$1 code=$2 file=$3 detail=''
  if [[ $code != 000 && -s $file ]]; then
    detail=$(head -c 512 "$file" | tr '\r\n' '  ')
  fi
  printf '%s (HTTP %s)%s\n' "$operation" "$code" "${detail:+ $detail}" >&2
}

# Invoked by the EXIT trap.
# shellcheck disable=SC2317
cleanup() {
  local result=$1 code=000 attempt remaining deadline
  trap - EXIT
  trap '' INT TERM
  if [[ -n $child ]]; then kill "$child" 2>/dev/null || :; fi
  if [[ -z $id ]] && ! $post_reported; then
    id=$(accepted_location)
    if [[ -n $id ]]; then
      echo "Stack ID: $id" >&2
    else
      echo 'POST was interrupted before confirming the stack ID. Check launcher records before resubmitting.' >&2
    fi
  fi
  if [[ -n $id ]]; then
    echo "Removing stack $id" >&2
    deadline=$((SECONDS + cleanup_timeout))
    for ((attempt=1; SECONDS<deadline; attempt++)); do
      remaining=$((deadline - SECONDS))
      if ((remaining <= 0)); then break; fi
      # The server runs teardown synchronously under this request's context.
      code=$(curl "${curl_options[@]}" --connect-timeout "$request_timeout" --max-time "$remaining" -o "$tmp/delete" -w '%{http_code}' -X DELETE "$base/stacks/$id") || code=000
      if [[ $code == 204 || $code == 404 ]]; then break; fi
      http_failure "DELETE $id attempt $attempt" "$code" "$tmp/delete"
      if [[ $code != 000 && $code != 409 && $code != 5?? ]]; then break; fi
      if ((SECONDS + cleanup_interval >= deadline)); then break; fi
      sleep "$cleanup_interval"
    done
    if [[ $code != 204 && $code != 404 ]]; then
      printf 'Cleanup failed. Retry with curl --disable -i -X DELETE %q\n' "$base/stacks/$id" >&2
      if ((result == 0)); then result=1; fi
    else
      echo "Stack $id removed" >&2
    fi
  fi
  rm -rf -- "$tmp"
  exit "$result"
}

trap 'cleanup "$?"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
# Never retry POST: a lost response can still mean a stack was created.
code=$(curl "${curl_options[@]}" --max-time "$request_timeout" -D "$tmp/headers" -o "$tmp/body" -w '%{http_code}' -H 'Content-Type: application/json' --data-binary @- "$base/stacks") || code=000
if [[ $code == 202 ]]; then
  id=$(jq -rs --arg pattern "$id_pattern" 'select(length == 1) | .[0].id | strings | select(length <= 63 and test($pattern) and (test("[^a-z0-9-]") | not))' "$tmp/body" 2>/dev/null || :)
fi
if [[ -z $id ]]; then id=$(accepted_location); fi
post_reported=true
if [[ -z $id ]]; then
  http_failure 'Launch response did not provide a confirmed stack ID' "$code" "$tmp/body"
  if [[ $code != 4?? ]]; then echo 'Check launcher records before resubmitting.' >&2; fi
  exit 1
fi
echo "Stack ID: $id" >&2
deadline=$((SECONDS + poll_timeout))
while ((SECONDS < deadline)); do
  remaining=$((deadline - SECONDS))
  if ((remaining <= 0)); then break; fi
  if ((remaining > request_timeout)); then remaining=$request_timeout; fi
  code=$(curl "${curl_options[@]}" --max-time "$remaining" -o "$tmp/status" -w '%{http_code}' "$base/stacks/$id") || code=000
  if [[ $code != 200 ]]; then
    http_failure "Status request for $id failed" "$code" "$tmp/status"
    exit 1
  fi
  state=$(jq -r '.state // empty' "$tmp/status") || exit 1
  case $state in
    ready)
      if ! STACK_URL=$(jq -er '.url | strings | select(test("^https?://[^/]+$"))' "$tmp/status"); then
        echo "Ready stack $id has no valid app URL" >&2
        exit 1
      fi
      export STACK_URL
      echo "Stack ready: $STACK_URL" >&2
      "$@" & child=$!
      wait "$child"; result=$?; child=
      exit "$result" ;;
    launching|starting) ;;
    *)
      echo "Stack $id ended in state $state: $(jq -r '[.failure_phase // "", .failure // ""] | join(": ")' "$tmp/status")" >&2
      exit 1 ;;
  esac
  remaining=$((deadline - SECONDS))
  if ((remaining <= 0)); then break; fi
  if ((remaining > poll_interval)); then remaining=$poll_interval; fi
  sleep "$remaining"
done
echo "Stack $id did not become ready within $poll_timeout seconds" >&2
exit 1
