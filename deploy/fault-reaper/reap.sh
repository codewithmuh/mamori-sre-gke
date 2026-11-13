#!/bin/sh
# fault-reaper: delete fault objects in namespace boutique whose mamori.dev/expires-at has passed.
# tools/gameday stamps that annotation on everything it applies and normally removes the objects
# itself. This catches the runs where it could not: the terminal closed, the laptop slept, the
# process was killed. Istio objects and Deployments have no TTL of their own. Objects without the
# annotation (a discovery run's, for example) are left alone, and a ValidatingAdmissionPolicy only
# lets this job delete objects labelled mamori.dev/fault=true that carry the annotation.
# It runs in the kubectl image, which has no shell; an init container copies busybox in.
set -u
bb=/tools/busybox
ns=boutique
# Seconds past expiry before reaping, so a live `gameday start` removes its own objects first and
# its timeline records the right reason.
grace=120
kinds="virtualservices.networking.istio.io sidecars.networking.istio.io
authorizationpolicies.security.istio.io deployments.apps podchaos.chaos-mesh.org
networkchaos.chaos-mesh.org stresschaos.chaos-mesh.org"

cutoff=$($bb date -u -d "@$(($($bb date -u +%s) - grace))" +%Y-%m-%dT%H:%M:%SZ)

# One JSON line per event, with Cloud Logging's field names.
log() {
  printf '{"severity":"%s","event":"%s","message":"%s","object":"%s","cutoff":"%s"}\n' \
    "$1" "$2" "$3" "${4:-}" "$cutoff"
}

status=0
for kind in $kinds; do
  if ! out=$(kubectl get "$kind" -n "$ns" -l mamori.dev/fault=true \
    -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.metadata.annotations.mamori\.dev/expires-at}{"\n"}{end}' 2>&1); then
    case "$out" in
      # The CRD is not installed (for example Chaos Mesh is not deployed): nothing to reap.
      *"doesn't have a resource type"*) continue ;;
    esac
    log ERROR list_failed "could not list $kind" "$kind"
    status=1
    continue
  fi
  while read -r name expires; do
    [ -n "$name" ] || continue
    case "$expires" in
      ????-??-??T??:??:??Z) ;;
      *) continue ;; # no expiry, or not in the form gameday writes: not ours to judge
    esac
    # Same-format UTC timestamps sort as strings.
    if $bb awk -v e="$expires" -v c="$cutoff" 'BEGIN { exit !(e < c) }'; then
      if kubectl delete "$kind/$name" -n "$ns" --wait=false >/dev/null 2>&1; then
        log WARNING reaped "deleted a fault object that expired at $expires" "$kind/$name"
      else
        log ERROR delete_failed "could not delete a fault object that expired at $expires" "$kind/$name"
        status=1
      fi
    fi
  done <<END
$out
END
done
exit "$status"
