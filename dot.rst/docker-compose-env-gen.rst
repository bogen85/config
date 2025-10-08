.. code-block:: text

   Short answer: VS Code/Dev Containers doesn’t natively read TOML to fill ${VAR} in your Compose files.
   Compose variable substitution happens **before** containers start, using only the host process env
   and a `.env` file in the Compose project directory. So you’ve got two practical patterns:

   ==========================================================
   Pattern A — Generate a `.env` from TOML *before* Compose runs
   ==========================================================
   Use `initializeCommand` in `devcontainer.json` (runs on the host, before the containers are created)
   to convert your TOML profile into a `.env` that lives next to your Compose file(s). Compose will
   then pick up those vars for `${VAR}` interpolation.

   Example layout
   --------------

   .. code-block:: text

      .yourrepo/
        .devcontainer/
          devcontainer.json
          compose.yml
          env.toml
          gen-env.py

   env.toml
   --------
   .. code-block:: toml

      [default]
      REGISTRY = "registry.example.com"
      IMAGE_TAG = "latest"

      [profiles.dev]
      IMAGE_TAG = "dev"
      FOO = "bar"

      [profiles.ci]
      IMAGE_TAG = "ci"
      FOO = "baz"

   gen-env.py
   ----------
   .. code-block:: python

      #!/usr/bin/env python3
      import argparse, os, sys
      try:
          import tomllib  # py311+
      except Exception:
          print("Python 3.11+ required (tomllib).", file=sys.stderr)
          sys.exit(1)

      p = argparse.ArgumentParser()
      p.add_argument("--toml", required=True)
      p.add_argument("--profile", default="default")
      p.add_argument("--out", required=True)
      args = p.parse_args()

      with open(args.toml, "rb") as f:
          data = tomllib.load(f)

      env = dict(data.get("default", {}))
      env.update(data.get("profiles", {}).get(args.profile, {}))

      with open(args.out, "w") as f:
          for k, v in env.items():
              f.write(f"{k}={v}\n")
      print(f"Wrote {args.out} for profile {args.profile}")

   devcontainer.json
   -----------------
   .. code-block:: json

      {
        "name": "my-devcontainer",
        "dockerComposeFile": [
          ".devcontainer/compose.yml"
        ],
        "service": "app",
        "workspaceFolder": "/workspace",
        "initializeCommand": "python3 .devcontainer/gen-env.py --toml .devcontainer/env.toml --profile ${localEnv:DEV_PROFILE-dev} --out .devcontainer/.env",
        "runServices": ["app"],
        "overrideCommand": false
      }

   compose.yml (excerpt)
   ---------------------
   .. code-block:: yaml

      services:
        app:
          image: ${REGISTRY}/myapp:${IMAGE_TAG}
          # ... rest ...

   Notes
   -----
   * The `.env` **must** be in the Compose project dir (same dir as `compose.yml`) for `${VAR}` interpolation.
     Since we referenced `.devcontainer/compose.yml`, writing `.devcontainer/.env` is perfect.
   * You can pick the profile via an environment variable on the host that starts VS Code:
     `DEV_PROFILE=ci code .` (the `${localEnv:...}` in `devcontainer.json` reads it).

   This approach works equally for Docker and Podman (with podman’s Docker socket compatibility);
   VS Code just ends up calling `compose` with your generated `.env`.

   =======================================================
   Pattern B — Use `env_file:` for container env (not for interpolation)
   =======================================================
   If you only need the variables **inside** the container (not to interpolate image names, volumes, etc.), you can:
   1. Keep your TOML → `.env` generation (use `onCreateCommand` or `postStartCommand` if you really don’t need it before Compose starts), and
   2. Point services at it via Compose:

   .. code-block:: yaml

      services:
        app:
          env_file:
            - .devcontainer/.env

   Caveat: `env_file:` **does not** participate in Compose’s `${VAR}` interpolation of the YAML itself—only the container
   process environment. So use Pattern A when you need to substitute in the Compose file
   (e.g., image tags, bind paths, replicas, etc.).

   Extras / Tips
   -------------
   * You can also use `direnv` on the host + the VS Code Direnv extension to populate the host environment,
     but a generated `.env` is the most predictable for Compose.
   * `containerEnv` / `remoteEnv` in `devcontainer.json` are great for setting variables **after** the container is up,
     but they won’t help with Compose-time `${VAR}`.
   * For Podman, make sure VS Code is pointed at the Podman socket
     (`DOCKER_HOST=unix:///run/user/1000/podman/podman.sock`) or set
     the VS Code “Dev Containers: Docker Path” to `podman`.

   TL;DR
   -----
   VS Code can’t read TOML for Compose interpolation. Generate a `.env` from your TOML **before** the devcontainer
   brings services up (with `initializeCommand`), place it next to the Compose file, and you’ll get exactly the
   “few composes + many config profiles” workflow you want—without duplicating Compose YAML.
