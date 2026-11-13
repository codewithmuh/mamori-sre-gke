#!/usr/bin/env bash
# Snooze every alert policy that slo-gen generated (user label generator=slo-gen) for N minutes, so a
# planned outage does not page: `make down` (the prober disappears before terraform destroy), or
# maintenance. Snoozed policies still evaluate; they just don't notify. Used by `make slo-snooze`.
#
# Usage: tools/slo-gen/snooze.sh PROJECT_ID [MINUTES] [REASON]   (MINUTES default 60)
set -euo pipefail

project=${1:?usage: $0 PROJECT_ID [MINUTES] [REASON]}
minutes=${2:-60}
reason=${3:-planned work}

if ! [[ $minutes =~ ^[1-9][0-9]*$ ]]; then
  echo "error: MINUTES must be a positive whole number, got '${minutes}'" >&2
  exit 2
fi

# Captured first so a gcloud failure stops the script (set -e); bash 3.2 (macOS) has no mapfile.
listed=$(gcloud monitoring policies list --project="$project" \
  --filter='userLabels.generator=slo-gen' --format='value(name)')
policies=()
while IFS= read -r name; do
  if [[ -n $name ]]; then policies+=("$name"); fi
done <<<"$listed"
if [[ ${#policies[@]} -eq 0 ]]; then
  echo "No slo-gen alert policies in ${project}; nothing to snooze."
  exit 0
fi

# The API wants a start no earlier than now; GNU date first, then BSD (macOS) date.
start=$(date -u +%Y-%m-%dT%H:%M:%SZ)
end=$(date -u -d "+${minutes} minutes" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null ||
  date -u -v+"${minutes}"M +%Y-%m-%dT%H:%M:%SZ)

# A snooze takes at most 16 policies.
batch=16
for ((i = 0; i < ${#policies[@]}; i += batch)); do
  chunk=$(IFS=,; echo "${policies[*]:i:batch}")
  gcloud monitoring snoozes create --project="$project" \
    --display-name="slo-gen: ${reason} (part $((i / batch + 1)))" \
    --criteria-policies="$chunk" --start-time="$start" --end-time="$end" >/dev/null
done
echo "Snoozed ${#policies[@]} slo-gen alert policies in ${project} until ${end} (${reason})."
