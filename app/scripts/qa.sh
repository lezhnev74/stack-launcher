#!/usr/bin/env bash
set -euo pipefail

check_format() {
  local unformatted
  unformatted=$(gofmt -l cmd internal)
  if [[ -n "$unformatted" ]]; then
    printf 'Run ./dev format for:\n%s\n' "$unformatted"
    return 1
  fi
}

merge_coverage_blocks() {
  # Cross-package test binaries can report the same block more than once.
  awk '
    NR == 1 { print; next }
    !($1 in statements) {
      blocks[++n] = $1
      statements[$1] = $2
    }
    { hits[$1] += $3 }
    END {
      for (i = 1; i <= n; i++) {
        block = blocks[i]
        printf "%s %d %.0f\n", block, statements[block], hits[block]
      }
    }
  ' "$1"
}

check_internal_coverage() {
  local module
  module=$(go list -m)
  awk -v prefix="$module/internal/" '
    index($1, prefix) == 1 {
      statements += $2
      if ($3 > 0) covered += $2
    }
    END {
      if (statements == 0) {
        print "No internal statements in the coverage profile."
        exit 1
      }
      printf "Internal statement coverage: %.2f%% (%d/%d), minimum 90%%\n", 100 * covered / statements, covered, statements
      exit (100 * covered < 90 * statements)
    }
  ' "$1"
}

check_go_tests() {
  local profile="$output/go.out" raw="$output/go.raw.out" status=0
  go test -race -shuffle=on -count=1 -covermode=atomic -coverpkg=./... -coverprofile="$raw" ./...
  merge_coverage_blocks "$raw" > "$profile"
  rm -- "$raw"
  go tool cover -func="$profile" > "$output/coverage.txt"
  check_internal_coverage "$profile" || status=1
  gocrap -c "$profile" -exclude '*_test.go' -max 7 -v ./... || status=1
  return "$status"
}

run_fuzz() {
  local package=internal/httpserver target=FuzzHealthz workspace status=0
  if ! go test -list="^$target$" "./$package" | grep -Fx -- "$target" >/dev/null; then
    printf 'Expected fuzz target %s in %s\n' "$target" "$package" >&2
    return 1
  fi

  workspace=$(mktemp -d)
  cp -R . "$workspace"

  # Go saves newly failing inputs under the package's testdata directory.
  (cd -- "$workspace" && go test -run='^$' -fuzz="^$target$" \
    -fuzztime="${FUZZ_TIME:-5s}" -fuzzminimizetime=5s -parallel=2 "./$package") || status=$?
  if (( status != 0 )) && [[ -d "$workspace/$package/testdata/fuzz/$target" ]]; then
    mkdir -p "$output/corpus/$package/testdata/fuzz"
    cp -R "$workspace/$package/testdata/fuzz/$target" "$output/corpus/$package/testdata/fuzz/"
  fi
  return "$status"
}

smoke_build() (
  local server_pid status fixture db
  fixture=$(mktemp -d)
  trap 'rm -rf -- "$fixture"' EXIT
  mkdir -m 700 "$fixture/state"
  printf 'listen: 127.0.0.1:18081\nstate_dir: state\n' > "$fixture/config.yml"
  cd "$fixture"
  "$output/stack-launcher" -c config.yml > "$output/server.log" 2>&1 &
  server_pid=$!
  trap 'kill "$server_pid" 2>/dev/null || true; wait "$server_pid" 2>/dev/null || true; rm -rf -- "$fixture"' EXIT

  status=$(curl --noproxy '*' --silent --show-error --output /dev/null --write-out '%{http_code}' \
    --connect-timeout 1 --max-time 2 --retry 5 --retry-connrefused --retry-delay 1 \
    http://127.0.0.1:18081/healthz)
  kill -0 "$server_pid"
  printf 'Built binary GET /healthz returned HTTP %s, expected 204\n' "$status"
  [[ "$status" == 204 ]]
  db="$fixture/state/stacks.sqlite3"
  if [[ ! -f "$db" || ! -s "$db" ]]; then
    echo 'Built binary did not initialize stacks.sqlite3 in state_dir.'
    return 1
  fi
)

record_result() {
  local status=$1 gate=$2 output=$3
  echo "$status" > "$output/status.txt"
  echo "[$gate] exit $status" >&3
}

run_gate() (
  gate=$1
  output="/results/$gate"
  mkdir -p "$output"
  echo "[$gate] started"
  exec 3>&1 > "$output/output.log" 2>&1
  trap 'record_result "$?" "$gate" "$output"' EXIT

  case "$gate" in
    lint)
      check_format
      shellcheck dev scripts/*.sh
      golangci-lint config verify
      golangci-lint run ./...
      go mod tidy -diff
      ;;
    test-go) check_go_tests ;;
    fuzz) run_fuzz ;;
    build)
      go build -trimpath -o "$output/stack-launcher" ./cmd/app
      smoke_build
      ;;
    security) govulncheck ./... ;;
  esac
)

if [[ "$1" == check ]]; then
  run_gate lint
  pids=()
  for gate in test-go fuzz build security; do
    run_gate "$gate" &
    pids+=("$!")
  done
  result=0
  for pid in "${pids[@]}"; do
    wait "$pid" || result=1
  done
  exit "$result"
fi

run_gate "$1"
