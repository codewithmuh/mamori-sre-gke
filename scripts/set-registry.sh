#!/usr/bin/env bash
# Points the image references under deploy/ at the lab's real Google Cloud project.
#
# Manifests name our own images asia-northeast1-docker.pkg.dev/PROJECT_ID/mamori/<name>:<tag>,
# because the project ID isn't known when they are written. This replaces the PROJECT_ID part of
# that registry path, and nothing else: a PROJECT_ID elsewhere (an environment variable name, a
# comment) stays as it is.
#
# Dry run by default: prints every line that would change. --apply rewrites the files; review
# them with `git diff` and commit, since Argo CD deploys what is in Git.
#
# Usage: scripts/set-registry.sh [--apply] [--from OLD_PROJECT_ID] [PROJECT_ID]
#   PROJECT_ID  project to point at; defaults to $PROJECT_ID (for example from .env)
#   --from      project ID the files name now; defaults to the placeholder PROJECT_ID. Use it to
#               move the references to a new project.
#   DEPLOY_DIR  directory to rewrite (default: deploy)
set -euo pipefail

usage() {
  sed -n '2,/^set -euo/p' "$0" | sed -e '$d' -e 's/^# \{0,1\}//'
}

apply=false
from=PROJECT_ID
project=${PROJECT_ID:-}
dir=${DEPLOY_DIR:-deploy}

while (($#)); do
  case $1 in
    --apply) apply=true ;;
    --from)
      [[ $# -ge 2 ]] || { echo "error: --from needs a project ID" >&2; exit 2; }
      from=$2
      shift
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    -*)
      echo "error: unknown option $1" >&2
      usage >&2
      exit 2
      ;;
    *) project=$1 ;;
  esac
  shift
done

# Same rule as the project_id variable in infra/: 6-30 characters, lowercase letters, digits and
# hyphens, starting with a letter. It also keeps the values safe to use in a search pattern.
project_id_re='^[a-z][a-z0-9-]{4,28}[a-z0-9]$'
if [[ -z $project ]]; then
  echo "error: no project ID. Pass it as an argument or set PROJECT_ID (see .env.example)." >&2
  exit 2
fi
if [[ ! $project =~ $project_id_re || $project == PROJECT_ID ]]; then
  echo "error: '${project}' is not a Google Cloud project ID" >&2
  exit 2
fi
if [[ $from != PROJECT_ID && ! $from =~ $project_id_re ]]; then
  echo "error: --from '${from}' is not a Google Cloud project ID" >&2
  exit 2
fi
if [[ ! -d $dir ]]; then
  echo "error: ${dir}/ not found; run from the repository root or set DEPLOY_DIR" >&2
  exit 2
fi

old="asia-northeast1-docker.pkg.dev/${from}/mamori/"
new="asia-northeast1-docker.pkg.dev/${project}/mamori/"
if [[ $old == "$new" ]]; then
  echo "The files already point at ${project}; nothing to do."
  exit 0
fi
export OLD=$old NEW=$new

files=()
while IFS= read -r -d '' file; do
  files+=("$file")
done < <(grep -rlIF --null -- "$old" "$dir" | sort -z)

if ((${#files[@]} == 0)); then
  echo "No image references to ${old} under ${dir}/; nothing to do."
  exit 0
fi

# perl, not sed -i, because sed -i differs between GNU and macOS. \Q...\E matches the text literally.
lines=$(grep -hcF -- "$old" "${files[@]}" | awk '{ n += $1 } END { print n }')
if [[ $apply == true ]]; then
  perl -pi -e 's/\Q$ENV{OLD}\E/$ENV{NEW}/g' -- "${files[@]}"
  echo "Rewrote ${old} -> ${new} (lines: ${lines}, files: ${#files[@]})."
  echo "Review with 'git diff -- ${dir}' and commit; Argo CD deploys from Git."
else
  perl -ne 'print "$ARGV:$.: $_" if s/\Q$ENV{OLD}\E/$ENV{NEW}/g; close ARGV if eof' -- "${files[@]}"
  echo
  echo "Dry run: ${old} -> ${new} would change the lines above (lines: ${lines}, files: ${#files[@]})."
  echo "Run again with --apply to write them."
fi
