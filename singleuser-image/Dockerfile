# The ODE singleuser image (SPEC §5.6 item 1).
#
# Offered as an additional KubeSpawner profile rather than replacing the default
# one, so that existing notebook users keep the image they have. The profile
# itself is a KubeSpawner profile, declared in the cluster's own configuration.
#
# Built and pushed by .github/workflows/singleuser.yml. Build it by hand with:
#
#   docker build -t ode-singleuser singleuser-image
#
# Licences of what this adds:
#   analytics-operator-lib-python  Apache-2.0
#   uv                             MIT OR Apache-2.0
#   ray                            Apache-2.0
#   mlflow                         Apache-2.0
#   confluent-kafka                Apache-2.0
#   pandas, numpy, scipy           BSD-3-Clause (from the base image)
#   psycopg2-binary                LGPL-3.0-or-later, with the OpenSSL exception
#
# The last one is the exception to "permissive only" and was decided rather than
# overlooked: Operator Lib pins it, it is imported at runtime rather than linked
# into anything of ours, and no ODE code touches it. The reasoning is recorded in
# .claude/license-policy.env, which is what lets the licence hook past it.

# Why this base, and why Python 3.12 rather than the current 3.13.
#
# quay.io/jupyter/scipy-notebook already carries jupyter_server, ipykernel,
# pandas, numpy, scipy and matplotlib, with the jovyan uid the chart's PVC is
# owned by — so the analysis half of §5.6 item 1 comes for free.
#
# The Python version is not a preference. Operator Lib pins confluent-kafka
# 2.4.0, which publishes no cp313 wheel, so on a 3.13 base pip falls back to
# building it from source and needs librdkafka headers that are not there. This
# is the last 3.12 tag the project published, and it is immutable — an unpinned
# base changes a developer's Python under them between two sessions.
#
# When Operator Lib moves off that pin, move this forward.
FROM quay.io/jupyter/scipy-notebook:python-3.12.11

USER root

# git is needed twice over. It installs Operator Lib and four of its dependencies,
# which are all git references rather than releases on PyPI — and since M7 it is
# also what ODE drives for the repo working copy of §5.11: clone, status, commit and
# push all run as `git` in this image, under the developer's own GitHub credential.
RUN apt-get update \
 && apt-get install -y --no-install-recommends git \
 && rm -rf /var/lib/apt/lists/*

USER ${NB_UID}

# uv, because the scaffolded repository is a uv project and the developer works on
# it from this pod. Its README asks for two commands — `uv lock` before the first
# experiment and `uv run --extra dev pytest` for the tests — and neither the base
# image nor anything above ships uv, so both were instructions the pod could not
# carry out. ODE runs `uv lock` itself at the end of a scaffold (pkg/repo), which
# is the same binary; a developer changing a dependency afterwards runs it by hand.
#
# Installed before the Operator Lib layer on purpose: ARG OPERATOR_LIB_REF
# invalidates everything below it, and this does not have to be rebuilt because the
# library moved.
RUN pip install --no-cache-dir uv

# Operator Lib is not on PyPI, so it comes from the repository. It brings ray,
# mlflow, confluent-kafka and the rest with it, at the versions it pins — which
# is the point of D15's "latest": this image is versioned alongside Operator Lib
# and rebuilt on its release, rather than pinning those packages here and
# silently disagreeing with it.
#
# The workflow resolves this to a commit SHA before building, so an image can be
# traced back to the exact Operator Lib it contains. A branch name still works
# for a hand build.
#
# The default is the newest release; the floor is older, and the two are different
# questions. D15's "track latest" is why this sits at the newest. v1.6.1 stops
# MLOperator renaming a run it was handed in MLFLOW_RUN_ID, which is the experiment
# path and therefore the Ray cluster's image rather than this one; v1.6.0 before it
# reads the deployment's OpenTelemetry baggage into every log record. Neither is
# something ODE calls from a cell, which is why they move the default and leave the
# floor where it is.
#
# v1.5.0 remains the floor rather than a preference, and it got there for a reason
# worth reading. v1.4.0 made MLOperator.train_once() public, which the scaffold's
# train.py calls; an older image fails there with AttributeError, loudly.
#
# v1.5.0 is the one that must not be undershot quietly. It is where Config.ts_conn
# lost its compiled-in default and ts_wrapper_url appeared, and ODE sets the
# wrapper rather than a DSN so that a run reads history under the developer's own
# permission (SNRGY-4637). A v1.4.0 image does not know the field: it ignores it,
# falls back to the built-in DSN, and reads every series in the instance without
# anything in the run saying so.
ARG OPERATOR_LIB_REF=v1.6.1

RUN pip install --no-cache-dir \
      "git+https://github.com/SENERGY-Platform/analytics-operator-lib-python@${OPERATOR_LIB_REF}"

# A small helper so a developer does not have to rebuild the platform clients in
# every cell. ODE pushes SENERGY_TOKEN and the two URLs into the kernel at
# session start and on every token refresh (§5.6 item 4); this reads them.
COPY --chown=${NB_UID}:${NB_GID} ode_platform.py /opt/ode/ode_platform.py
# Assigned rather than prepended to ${PYTHONPATH}, which the base image does not
# set: the expansion would leave a trailing colon, and an empty PYTHONPATH entry
# means the working directory. That would put a developer's workspace on
# sys.path, where a file called random.py shadows the standard library.
ENV PYTHONPATH=/opt/ode

# The workspace ODE runs kernels in. Set here as well so that a developer who
# opens JupyterLab directly finds the same path; the mount itself is the chart's,
# and this is only the path inside it.
ENV ODE_WORKSPACE=data/ode

# Traceability: which Operator Lib this image actually contains. Overwritten by
# the workflow with the resolved SHA.
LABEL org.opencontainers.image.source="https://github.com/SENERGY-Platform/operator-development-environment"
LABEL org.opencontainers.image.description="JupyterHub singleuser image for the Operator Development Environment"
