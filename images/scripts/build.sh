#!/bin/bash
# CrowdVM image build entry point. cloud-init runs it once (runcmd) inside the
# build VM: build.sh <minimal|full>. Results go to the second serial port
# (/dev/ttyS1), which the builder reads from a file on the host:
#   tool=version lines, then CROWDVM-BUILD-OK (or CROWDVM-BUILD-FAILED ...).
# The VM powers off at the end either way.
set -Eeuo pipefail
profile=${1:?profile}
here=$(cd "$(dirname "$0")" && pwd)
result=/dev/ttyS1

finish() {
	sync
	systemctl poweroff --no-block || poweroff -f
}
fail() {
	echo "CROWDVM-BUILD-FAILED line $1: $2" | tee "$result" >&2
	finish
	exit 1
}
trap 'fail $LINENO "$BASH_COMMAND"' ERR

export DEBIAN_FRONTEND=noninteractive
export HOME=${HOME:-/root} # cloud-init runcmd sets none (composer, npm, uv need it)
step() { echo "=== crowdvm build [$profile]: $*"; }

step minimal
bash "$here/minimal.sh"
if [ "$profile" = full ]; then
	step full
	bash "$here/full.sh"
fi
step tools
bash "$here/tools.sh" "$profile" | tee /var/lib/crowdvm-tools.txt
step finalize
bash "$here/finalize.sh"
{ cat /var/lib/crowdvm-tools.txt; echo CROWDVM-BUILD-OK; } >"$result"
rm -f /var/lib/crowdvm-tools.txt
step done
finish
