#!/bin/bash -xe

# Copyright (C) 2026, Pelican Project, Morgridge Institute for Research
#
# Licensed under the Apache License, Version 2.0 (the "License"); you
# may not use this file except in compliance with the License.  You may
# obtain a copy of the License at
#
#    http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#

# This tests that an origin's health reflects the storage clients actually read.
#
# The failure it guards against is the one health testing existed to catch and
# could not: on an XRootD-fronted origin, /pelican/monitoring resolves under
# Origin.RunLocation while real exports are symlinks to the administrator's
# volume. A probe confined to the monitoring namespace therefore keeps passing
# after the data volume becomes unreadable, and the origin goes on advertising
# "ok" while every client request fails.
#
# The test:
#   1. Stand up director + registry + origin over a real storage directory.
#   2. Confirm a client can read an object and the Director reports the origin healthy.
#   3. Break the *real* export -- chmod 000 -- leaving the run location untouched.
#   4. Confirm the client read now fails.
#   5. Confirm the Director notices, and that its health detail names a reason.
#
# Step 5 is the assertion that matters. Steps 2-4 establish that the break is
# real and client-visible, so a failure at step 5 is unambiguously the health
# system failing to see it rather than the test failing to break anything.

set -e

TEST_ROOT="$(mktemp -d "/tmp/pel-health-storage.XXXXXX")"
chmod 755 "${TEST_ROOT}"

FED_CONFIG_DIR="${TEST_ROOT}/fed_cfg"
FED_RUNTIME_DIR="${TEST_ROOT}/fed_rt"
ORIGIN_DIR="${TEST_ROOT}/origin"
ORIGIN_RUN="${TEST_ROOT}/orun"

mkdir -p "${FED_CONFIG_DIR}" "${FED_RUNTIME_DIR}" "${ORIGIN_DIR}" "${ORIGIN_RUN}"
chmod 755 "${FED_CONFIG_DIR}" "${FED_RUNTIME_DIR}" "${ORIGIN_RUN}"
chmod 777 "${ORIGIN_DIR}"
if [ "$(id -u)" -eq 0 ]; then
    chown xrootd: "${ORIGIN_DIR}" || true
fi

echo "fake-oidc-secret" > "${FED_CONFIG_DIR}/oidc-secret"

export PELICAN_TLSSKIPVERIFY=true
export PELICAN_SERVER_ENABLEUI=false
export PELICAN_ORIGIN_ENABLEPUBLICREADS=true
# There is no cache in this federation, so the Director must be willing to fall
# back to the origin for a client read.
export PELICAN_ORIGIN_ENABLEDIRECTREADS=true
export PELICAN_ORIGIN_ENABLEVOMS=false
export PELICAN_REGISTRY_REQUIRECACHEAPPROVAL=false
export PELICAN_REGISTRY_REQUIREORIGINAPPROVAL=false
export PELICAN_LOGGING_LEVEL=debug

# Probe often so the test does not spend minutes waiting for a cycle.
export PELICAN_ORIGIN_SELFTESTINTERVAL=2s
export PELICAN_DIRECTOR_ORIGINCACHEHEALTHTESTINTERVAL=2s
# Health reaches the Director only on the advertisement cycle, which defaults to
# a minute. Without shortening it the test would only ever see the origin's first
# advertisement, taken while it was still initializing.
export PELICAN_SERVER_ADVERTISEMENTINTERVAL=3s

export PELICAN_CONFIGBASE="${FED_CONFIG_DIR}"
export PELICAN_RUNTIMEDIR="${FED_RUNTIME_DIR}"
export PELICAN_SERVER_DBLOCATION="${FED_CONFIG_DIR}/registry.sql"
export PELICAN_SERVER_WEBPORT=0
export PELICAN_ORIGIN_PORT=0
export PELICAN_ORIGIN_RUNLOCATION="${ORIGIN_RUN}"
export PELICAN_OIDC_CLIENTID="sometexthere"
export PELICAN_OIDC_CLIENTSECRETFILE="${FED_CONFIG_DIR}/oidc-secret"
# Deliberately not setting ORIGIN_FEDERATIONPREFIX / ORIGIN_STORAGEPREFIX: the
# single-export env vars conflict with an Exports block, and the block is what
# carries HealthProbeObject.
# A natively-served backend. Real-export probing -- the thing under test -- is on
# by default here because Pelican authorizes these requests itself; on
# XRootD-fronted backends it is gated off pending an unresolved scitokens/authfile
# question, so this scenario is undetectable there by design.
export PELICAN_ORIGIN_STORAGETYPE=posixv2

# The object a client reads, and the object the health probe reads. Naming one
# explicitly is what lets a read-only export be probed at all.
echo "hello-storage-health" > "${ORIGIN_DIR}/data.txt"
echo "probe" > "${ORIGIN_DIR}/health-probe.txt"
chmod 644 "${ORIGIN_DIR}/data.txt" "${ORIGIN_DIR}/health-probe.txt"

cat > "${FED_CONFIG_DIR}/origin.yaml" <<YAML
Origin:
  Exports:
    - StoragePrefix: ${ORIGIN_DIR}
      FederationPrefix: /test
      HealthProbeObject: health-probe.txt
      Capabilities: ["PublicReads", "Reads", "Listings", "DirectReads"]
YAML
export PELICAN_CONFIG="${FED_CONFIG_DIR}/origin.yaml"

cleanup() {
    echo "Cleaning up..."
    if [ -n "${FED_PID:-}" ]; then
        kill -SIGINT "${FED_PID}" 2>/dev/null || true
        sleep 1
        descendants=$(pgrep -P "${FED_PID}" 2>/dev/null) || true
        # shellcheck disable=SC2086
        kill -9 ${FED_PID} ${descendants} 2>/dev/null || true
    fi
    rm -rf "${TEST_ROOT}"
    echo "Cleanup done."
}
trap cleanup EXIT

wait_for_address_file() {
    local file="$1" pid="$2" tries=0
    while [ ! -f "${file}" ]; do
        if ! kill -0 "${pid}" 2>/dev/null; then
            echo "federation exited before writing its address file"; echo "TEST FAILED"; exit 1
        fi
        sleep 0.5; tries=$((tries + 1))
        if [ "${tries}" -gt 120 ]; then echo "no address file after 60s"; echo "TEST FAILED"; exit 1; fi
    done
}

wait_for_healthy() {
    local url="$1" tries=0
    while true; do
        code=$(curl -m 5 -k -s -o /dev/null -w "%{http_code}" "${url}") || true
        [ "${code}" = "200" ] && return 0
        sleep 0.5; tries=$((tries + 1))
        if [ "${tries}" -gt 120 ]; then echo "not healthy after 60s"; echo "TEST FAILED"; exit 1; fi
    done
}

# Poll the Director's view of the origin until a predicate holds, or give up.
# Returns the last body seen in DIRECTOR_BODY.
wait_for_director_state() {
    local url="$1" jq_filter="$2" want="$3" label="$4" tries=0
    while true; do
        DIRECTOR_BODY=$(curl -m 5 -k -s "${url}") || true
        got=$(echo "${DIRECTOR_BODY}" | jq -r "${jq_filter}" 2>/dev/null) || got=""
        if [ "${got}" = "${want}" ]; then
            echo "Director reports ${label}."
            return 0
        fi
        sleep 1; tries=$((tries + 1))
        if [ "${tries}" -gt 45 ]; then
            echo "Director never reported ${label} (last saw '${got}') after 45s"
            echo "Body: ${DIRECTOR_BODY}"
            echo "TEST FAILED"
            exit 1
        fi
    done
}

echo "=== 1. Start the federation ==="
./pelican-server serve --module director --module registry --module origin -d &
FED_PID=$!

FED_ADDR_FILE="${FED_RUNTIME_DIR}/pelican.addresses"
wait_for_address_file "${FED_ADDR_FILE}" "${FED_PID}"
# shellcheck source=/dev/null
source "${FED_ADDR_FILE}"
FED_WEB_URL="${SERVER_EXTERNAL_WEB_URL}"

wait_for_healthy "${FED_WEB_URL}/api/v1.0/health"

SERVERS_URL="${FED_WEB_URL}/api/v1.0/director_ui/servers"
ORIGIN_FILTER='.[] | select(.type == "Origin")'

echo "=== 2. A client can read the object, and the origin is not in error ==="
READ_BODY=$(curl -m 10 -k -sL "${FED_WEB_URL}/test/data.txt")
if [ "${READ_BODY}" != "hello-storage-health" ]; then
    echo "Baseline read failed; got '${READ_BODY}'"
    echo "TEST FAILED"
    exit 1
fi
echo "Baseline client read succeeded."

# A read-only export gives the Director no object of its own to probe in the
# monitoring namespace, so its own verdict is legitimately "Unknown". What must
# be healthy is the origin's self-report, which is what covers the real export.
wait_for_director_state "${SERVERS_URL}" \
    "[${ORIGIN_FILTER}] | .[0].serverStatus" "ok" "the origin healthy"

echo "=== 3. Break the real export, leaving the run location untouched ==="
# This is the crux. Origin.RunLocation still works, so the synthetic
# /pelican/monitoring namespace is entirely unaffected.
#
# Removing the tree is the only injection that actually breaks this backend, and
# the two obvious alternatives are both traps:
#
#   chmod 000  -- CI runs as root, and root bypasses file permissions.
#   mv         -- the backend holds an os.Root handle bound to the directory
#                 *inode*, not to its path, so a rename on the same filesystem
#                 leaves it serving the data perfectly happily. Nothing breaks,
#                 and a test built on it proves nothing while appearing to fail.
rm -rf "${ORIGIN_DIR}"

echo "=== 4. Confirm the break is real and client-visible ==="
CLIENT_FAILED=0
for _ in $(seq 1 20); do
    code=$(curl -m 10 -k -sL -o /dev/null -w "%{http_code}" "${FED_WEB_URL}/test/data.txt") || true
    if [ "${code}" != "200" ]; then CLIENT_FAILED=1; break; fi
    sleep 1
done
if [ "${CLIENT_FAILED}" -ne 1 ]; then
    echo "Client could still read after the export was made unreadable;"
    echo "the test failed to break anything, so step 5 would prove nothing."
    echo "TEST FAILED"
    exit 1
fi
echo "Client reads now fail, as expected."

echo "=== 5. The Director must notice, and say why ==="
# The assertion this whole script exists for. Before real-export probing, the
# origin kept reporting healthy here indefinitely.
wait_for_director_state "${SERVERS_URL}" \
    "[${ORIGIN_FILTER}] | .[0].serverStatus" "critical" "the origin unhealthy"

DETAIL=$(echo "${DIRECTOR_BODY}" | jq -r "[${ORIGIN_FILTER}] | .[0].healthDetail // empty")
if [ -z "${DETAIL}" ]; then
    echo "Director reported the origin unhealthy but attached no healthDetail."
    echo "Knowing *that* a server is broken without knowing *why* is the gap this work closes."
    echo "TEST FAILED"
    exit 1
fi

REASON=$(echo "${DETAIL}" | jq -r '.reported.selfTest.results[]? | select(.outcome == "fail") | .reason' | head -1)
if [ -z "${REASON}" ] || [ "${REASON}" = "null" ]; then
    echo "healthDetail carried no failure reason: ${DETAIL}"
    echo "TEST FAILED"
    exit 1
fi
echo "Director reported failure reason: ${REASON}"

echo "============================================"
echo "TEST PASSED"
echo "============================================"
