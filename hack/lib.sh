#!/usr/bin/env bash

# Copyright 2025 The KCP Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

### Contains commonly used functions for the other scripts.

# Required for signal propagation to work so
# the cleanup trap gets executed when a script
# receives a SIGINT
set -o monitor

# Get the operating system
# Possible values are:
#		* linux for linux
#		* darwin for macOS
#
# usage:
# if [ "${OS}" == "darwin" ]; then
#   # do macos stuff
# fi
OS="$(echo $(uname) | tr '[:upper:]' '[:lower:]')"

retry() {
  # Works only with bash but doesn't fail on other shells
  start_time=$(date +%s)
  set +e
  # We use an extra wrapping to write junit and have a timer
  retry_backoff $@
  rc=$?
  set -e
  elapsed_time=$(($(date +%s) - $start_time))
  write_junit "$rc" "$elapsed_time"
  return $rc
}

retry_backoff() {
  retries=$1
  shift

  count=0
  delay=1
  until "$@"; do
    rc=$?
    count=$((count + 1))
    if [ $count -lt "$retries" ]; then
      echo "Retry $count/$retries exited $rc, retrying in $delay seconds…" > /dev/stderr
      sleep $delay
    else
      echo "Retry $count/$retries exited $rc, no more retries left." > /dev/stderr
      return $rc
    fi
    delay=$((delay * 2))
  done
  return 0
}

retry_linear() {
  delay=$1
  retries=$2
  shift
  shift

  count=0
  until "$@"; do
    rc=$?
    count=$((count + 1))
    if [ $count -lt "$retries" ]; then
      echodate "[$count/$retries] Command returned $rc, retrying…"
      sleep $delay
    else
      echodate "Command returned $rc, no more retries left."
      return $rc
    fi
  done

  echodate "Command succeeded."

  return 0
}

echodate() {
  # do not use -Is to keep this compatible with macOS
  echo "[$(date +%Y-%m-%dT%H:%M:%S%:z)]" "$@" > /dev/stderr
}

write_junit() {
  # Doesn't make any sense if we don't know a testname
  if [ -z "${TEST_NAME:-}" ]; then return; fi
  # Only run in CI
  if [ -z "${ARTIFACTS:-}" ]; then return; fi

  rc=$1
  duration=${2:-0}
  errors=0
  failure=""
  if [ "$rc" -ne 0 ]; then
    errors=1
    failure='<failure type="Failure">Step failed</failure>'
  fi
  TEST_CLASS="${TEST_CLASS:-Kubermatic}"
  cat << EOF > ${ARTIFACTS}/junit.$(echo $TEST_NAME | sed 's/ /_/g' | tr '[:upper:]' '[:lower:]').xml
<?xml version="1.0" ?>
<testsuites>
  <testsuite errors="$errors" failures="$errors" name="$TEST_CLASS" tests="1">
    <testcase classname="$TEST_CLASS" name="$TEST_NAME" time="$duration">
      $failure
    </testcase>
  </testsuite>
</testsuites>
EOF
}

ensure_github_host_pubkey() {
  # check whether we already have a known_hosts entry for Github
  if ssh-keygen -F github.com > /dev/null 2>&1; then
    echodate "Github's SSH host key already present."
  else
    local github_rsa_key
    # https://help.github.com/en/github/authenticating-to-github/githubs-ssh-key-fingerprints
    github_rsa_key="github.com ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQCj7ndNxQowgcQnjshcLrqPEiiphnt+VTTvDP6mHBL9j1aNUkY4Ue1gvwnGLVlOhGeYrnZaMgRK6+PKCUXaDbC7qtbW8gIkhL7aGCsOr/C56SJMy/BCZfxd1nWzAOxSDPgVsmerOBYfNqltV9/hWCqBywINIR+5dIg6JTJ72pcEpEjcYgXkE2YEFXV1JHnsKgbLWNlhScqb2UmyRkQyytRLtL+38TGxkxCflmO+5Z8CSSNY7GidjMIZ7Q4zMjA2n1nGrlTDkzwDCsw+wqFPGQA179cnfGWOWRVruj16z6XyvxvjJwbz0wQZ75XK5tKSb7FNyeIEs4TT4jk+S4dhPeAUC5y+bDYirYgM4GC7uEnztnZyaVWQ7B381AK4Qdrwt51ZqExKbQpTUNn+EjqoTwvqNj4kqx5QUCI0ThS/YkOxJCXmPUWZbhjpCg56i+2aB6CmK2JGhn57K5mj0MNdBXA4/WnwH6XoPWJzK5Nyu2zB3nAZp+S5hpQs+p1vN1/wsjk="

    echodate "Adding Github's SSH host key to known hosts…"

    mkdir -p "$HOME/.ssh"
    chmod 700 "$HOME/.ssh"
    echo "$github_rsa_key" >> "$HOME/.ssh/known_hosts"
    chmod 600 "$HOME/.ssh/known_hosts"
  fi
}

vault_ci_login() {
  # already logged in
  if [ -n "${VAULT_TOKEN:-}" ]; then
    return 0
  fi

  # check environment variables
  if [ -z "${VAULT_ROLE_ID:-}" ] || [ -z "${VAULT_SECRET_ID:-}" ]; then
    echo "VAULT_ROLE_ID and VAULT_SECRET_ID must be set to programmatically authenticate against Vault."
    return 1
  fi

  local token
  token=$(vault write --format=json auth/approle/login "role_id=$VAULT_ROLE_ID" "secret_id=$VAULT_SECRET_ID" | jq -r '.auth.client_token')

  export VAULT_TOKEN="$token"
}

# append_trap appends to existing traps, if any. It is needed because Bash replaces existing handlers
# rather than appending: https://stackoverflow.com/questions/3338030/multiple-bash-traps-for-the-same-signal
# Needing this func is a strong indicator that Bash is not the right language anymore. Also, this
# basically needs unit tests.
append_trap() {
  command="$1"
  signal="$2"

  # Have existing traps, must append
  if [[ "$(trap -p | grep $signal)" ]]; then
    existingHandlerName="$(trap -p | grep $signal | awk '{print $3}' | tr -d "'")"

    newHandlerName="${command}_$(head /dev/urandom | tr -dc A-Za-z0-9 | head -c 13)"
    # Need eval to get a random func name
    eval "$newHandlerName() { $command; $existingHandlerName; }"
    echodate "Appending $command as trap for $signal, existing command was $existingHandlerName."
    trap $newHandlerName $signal
  # First trap
  else
    echodate "Using $command as trap for $signal."
    trap $command $signal
  fi
}

docker_logs() {
  if [[ $? -ne 0 ]]; then
    echodate "Printing Docker logs…"
    cat /tmp/docker.log
  fi
}

start_docker_daemon_ci() {
  # DOCKER_REGISTRY_MIRROR_ADDR is injected via Prow preset;
  # start-docker.sh is part of the build image.
  DOCKER_REGISTRY_MIRROR="${DOCKER_REGISTRY_MIRROR_ADDR:-}" DOCKER_MTU=1400 start-docker.sh

  # enable the modern buildx plugin
  echodate "Enabling dockerx plugin…"
  docker buildx install
}

start_docker_daemon() {
  if docker stats --no-stream > /dev/null 2>&1; then
    echodate "Not starting Docker again, it's already running."
    return
  fi

  # Start Docker daemon
  echodate "Starting Docker…"
  dockerd > /tmp/docker.log 2>&1 &

  echodate "Started Docker successfully."
  append_trap docker_logs EXIT

  # Wait for Docker to start
  echodate "Waiting for Docker…"
  retry 5 docker stats --no-stream
  echodate "Docker became ready."
}

repeat() {
  local end=$1
  local str="${2:-=}"

  for i in $(seq 1 $end); do
    echo -n "${str}"
  done
}

heading() {
  local title="$@"
  echo "$title"
  repeat ${#title} "="
  echo
}

# go_test wraps running `go test` commands. The first argument needs to be file name
# for a junit result file that will be generated if go-junit-report is present and
# $ARTIFACTS is set. The remaining arguments are passed to `go test`.
go_test() {
  local junit_name="${1:-}"
  shift

  # only run go-junit-report if binary is present and we're in CI / the ARTIFACTS environment is set
  if [ -x "$(command -v go-junit-report)" ] && [ ! -z "${ARTIFACTS:-}" ]; then
    go test "$@" 2>&1 | go-junit-report -set-exit-code -iocopy -out ${ARTIFACTS}/junit.${junit_name}.xml
  else
    go test "$@"
  fi
}
