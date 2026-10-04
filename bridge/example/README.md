# ipcr-hello

The IPCR Forge example. A tag on this repository builds its image, IPCR publishes it on IPFS,
and the repository is mirrored to Radicle.

## Try it

1. **Tag a release.** In Gitea: *Tags → New tag* (or *Releases → New release*), name it `v1.0.0`
   and target `main`. From a clone it works too:

   ```sh
   git tag v1.0.0 && git push origin v1.0.0
   ```

2. **Watch the build** under *Actions*. The first build on a new server takes a few minutes more,
   while it downloads its build images.

3. **Find the image** on the forge's page (`ipcr-forge-<your domain>`, *Published images*), about
   a minute after the build finishes. Then on this server or on any other running IPCR:

   ```yaml
   services:
     hello:
       image: ipcr.localhost:4767/ipns/<forge name>/gitea_admin/ipcr-hello:1.0.0
       ports: ["8080:8080"]
   ```

## What makes it work

- [`.gitea/workflows/build.yml`](.gitea/workflows/build.yml) pushes to `localhost:5000`, the
  forge's staging registry. IPCR imports every tag it finds there.
- [`Dockerfile`](Dockerfile) is an ordinary Dockerfile. Anything that builds works.

Copy the workflow into any public or private repository on this forge to publish it the same way.
