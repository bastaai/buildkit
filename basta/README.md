# Basta buildkitd image

`earthbuild/buildkitd:v0.8.18-b0385986` with only `/usr/bin/buildkitd` replaced by a build of this
branch (`fix/merged-edge-owner-jobs`): EarthBuild/buildkit@51fe8fb9 plus three upstream solver
backports and a port of moby/buildkit#7265, which fixes moby/buildkit#7268 ("inconsistent graph
state" on cancelled/merged edges).

The binary is built the way EarthBuild builds it (`Dockerfile` target `buildkitd`): Go 1.21.13,
`CGO_ENABLED=0`, `-mod=vendor`, tags `osusergo netgo static_build seccomp`, static ldflags. Version
is the stock `v0.8.18-b0385986` so earth sees no version mismatch; Revision is this repo's commit.
`buildkitd --version` tells the images apart.

## Published image

`.github/workflows/basta-buildkitd.yml` builds linux/arm64 and linux/amd64 and pushes:

- `ghcr.io/bastaai/buildkitd:v0.8.18-b0385986-fix7268`
- `ghcr.io/bastaai/buildkitd:v0.8.18-b0385986-fix7268-<short sha>`

Run it by pushing a `basta-buildkitd-*` tag or with "Run workflow" in the Actions tab.

## Building by hand

```sh
docker build -t basta/buildkitd:v0.8.18-fix7268 -f basta/Dockerfile \
  --build-arg REVISION=<sha> https://github.com/bastaai/buildkit.git#<sha>
```

A git-URL context has no `.git`, so `REVISION` is required there (or pass
`--build-arg BUILDKIT_CONTEXT_KEEP_GIT_DIR=1`). From a local checkout it is read from git.

## Using it with earth

```yaml
# ~/.earth/config.yml of every user that runs earth on the host (official earth uses installation name "earth")
global:
  buildkit_image: ghcr.io/bastaai/buildkitd:v0.8.18-b0385986-fix7268-<short sha>
```

`--buildkit-image` or `EARTH_BUILDKIT_IMAGE` override the config. Earth pulls the image only when it
is not present locally, and recreates `earth-buildkitd` when the running container's image ID differs
from the configured image; the `earth-cache` volume is kept. Prefer the `-<short sha>` tag, since a
moved tag that is already present locally is never re-pulled. Remove the key to go back to stock.
