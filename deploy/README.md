# Deployment artifacts

M4 needs two things outside this repository, and one thing built from it.

**Outside**, in `SENERGY-Platform/rancher-2-defs`, which is where the cluster's
JupyterHub is configured by Argo CD: ODE registered as a Hub service, and the
ODE image offered as an additional KubeSpawner profile.
[`jupyterhub/README.md`](jupyterhub/README.md) says exactly which files, which
scopes, and the one non-obvious thing about where the service token may come
from. The YAML is not duplicated here; a second copy would drift from the one
that is actually applied.

**From here**, the singleuser image:

| File | What it is |
|---|---|
| `singleuser/Dockerfile` | Operator Lib, and through it Ray and MLflow, on the official scipy notebook base |
| `singleuser/ode_platform.py` | A small reader for the platform token and URLs ODE pushes into a kernel, baked into that image |
| `../.github/workflows/singleuser.yml` | Builds it, checks it, and publishes it to `ghcr.io/senergy-platform/ode-singleuser` |

The image rebuilds when `deploy/singleuser/**` changes and on demand — the latter
being the trigger D15 names, "rebuild on an Operator Lib release". It is
deliberately not rebuilt on every push to master the way the backend image is:
nothing in it comes from ODE's Go code.

Two things about it are not obvious and are argued in the Dockerfile itself. The
base is pinned to **Python 3.12**, not the current 3.13, because Operator Lib
pins `confluent-kafka==2.4.0` and that publishes no cp313 wheel. And Ray and
MLflow are **not pinned here** — Operator Lib pins them, and restating the
versions would mean silently disagreeing with it. A local build resolves to
`operator-lib 1.3.3`, `ray 2.55.0`, `mlflow 3.8.1`, `pandas 2.3.1` on Python
3.12.11.

Neither the image nor the Hub configuration has been applied. What *has* been
verified against the live Hub is the ODE side — spawn, token minting, the kernel
protocol, the workspace on the PVC and the keep-alive — using a developer's own
token in place of a service credential. See "Trying M4" in the root README.

## Order of operations

1. Seal the service token and apply it, then merge the Hub configuration. The
   command is in `values-ode.yaml`; the hub pod cannot start while an env var
   references a secret that is not there yet, so the SealedSecret goes first.
2. Configure ODE: `jupyterhub_url`, and `jupyterhub_token` set to the same token.
   ODE refuses to start if that token is missing a scope, and warns if the
   credential is a user token rather than a service one.
3. Run the **Singleuser Image** workflow (Actions → Run workflow). It resolves
   Operator Lib to a commit, builds, checks the image can import what §5.6 item 1
   asks for, and only then publishes.
4. Take the tag from that run's job summary and set it in
   `values-ode-singleuser.yaml`, which is already listed in the Argo CD
   application and currently carries a placeholder. Then set
   `jupyterhub_profile: "ode"` in ODE's own configuration.

Steps 1 and 2 are enough to run code. Steps 3 and 4 are what make the code
useful — until then a developer gets the deployment's default image, which has no
pandas.

The profile list is live before the tag is, and that is safe: the default profile
keeps the current image, and KubeSpawner gives a spawn that names no profile the
one marked default, which is what ODE's API spawns send. Only a developer who
picks "ODE" from the chooser hits the placeholder, and it fails as an image pull
naming `REPLACE-WITH-A-PUBLISHED-TAG`.

## What is deliberately not here

**The NetworkPolicy on singleuser pods.** That is M10, and the risk register
calls it the only hard security prerequisite before external users. JupyterHub
isolates users from one another but does not restrict egress, and these pods run
developer- and LLM-authored code. Nothing in M4 changes that, and nothing in M4
pretends to.

**Anything that stops a pod.** ODE spawns and it lets go; it never deletes a
server. A pod is the developer's, their files are on it, and reclaiming it is the
cluster's idle culling — which ODE stops holding off once a session has been
quiet for `jupyterhub_idle_timeout`.
