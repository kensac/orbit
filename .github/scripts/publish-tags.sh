#!/usr/bin/env bash
# publish-tags.sh sha|latest IMAGE DIGEST SHA: point IMAGE:sha-<short SHA>,
# or IMAGE:latest, at IMAGE@DIGEST. latest is left alone when it already
# holds a newer main commit (CI runs finish out of order, and latest must
# never move back). Run only after DIGEST passed its smoke test on every
# architecture. Needs a full clone (it compares commits) and a registry
# login.
set -euo pipefail
kind=$1 image=$2 digest=$3 sha=$4
case $kind in
  sha) tag="$image:sha-${sha:0:7}" ;;
  latest)
    tag="$image:latest"
    cur=$(.github/scripts/latest-revision.sh "$image")
    if [ -n "$cur" ] && [ "$cur" != "$sha" ] && git merge-base --is-ancestor "$sha" "$cur" 2>/dev/null; then
      echo "publish: $tag holds $cur, a newer commit than $sha; leaving it"
      exit 0
    fi
    ;;
  *) echo "usage: publish-tags.sh sha|latest IMAGE DIGEST SHA" >&2; exit 2 ;;
esac
docker buildx imagetools create -t "$tag" "$image@$digest"
# A pin is the digest: the tag must name exactly the image that was tested.
got=$(docker buildx imagetools inspect "$tag" --format '{{ .Manifest.Digest }}')
[ "$got" = "$digest" ] || { echo "publish: $tag is $got, not the tested $digest"; exit 1; }
echo "publish: $tag -> $digest"
