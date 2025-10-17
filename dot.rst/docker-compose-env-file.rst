Profile-Scoped `env_file` for Passing Multiple Environment Variables
===================================================================

This guide shows how to use a **profile-specific** ``env_file`` in a
Compose project so you can inject **many** environment variables into a
container—without editing the Compose YAML for each profile.

Why this pattern
----------------
- You can **template the path** to the ``env_file`` with a profile variable.
- One file can contain **1..N** ``KEY=VALUE`` pairs.
- The variables become part of the **container’s config at create time**, so
  any plain ``docker|podman exec`` sees them automatically.
- If required inputs are missing, Compose can **fail early**.

Key distinctions
----------------
- Top-level ``.env`` (project env): used **only** for variable **substitution**
  in the Compose file.
- Service-level ``env_file``: injects variables into the **container runtime
  environment** at **create time**.
- Changing an ``env_file`` requires a **recreate** (not just restart).

Directory layout (example)
--------------------------
.. code-block:: text

   your-app/
   ├── compose.yaml
   ├── .env                      # optional, for templating, not injected
   ├── .passthru.dev.env         # profile-specific runtime env
   ├── .passthru.staging.env
   └── .passthru.prod.env

Compose file
------------
Use a **required** interpolation for the profile variable so Compose fails if it
is unset/empty.

.. code-block:: yaml

   # compose.yaml
   services:
     app:
       image: your/app:latest
       env_file:
         - .passthru.${DEV_CONTAINER_PROFILE:?set DEV_CONTAINER_PROFILE}.env
       # You may also keep a few fixed envs here:
       environment:
         STATIC_ONE: "always_here"

Profile variable
----------------
You can source the profile from your shell or from the project ``.env`` file.

**Shell (recommended)**

.. code-block:: bash

   export DEV_CONTAINER_PROFILE=dev
   podman compose up -d   # or: docker compose up -d

**Project .env (optional)**

.. code-block:: dotenv

   # .env
   DEV_CONTAINER_PROFILE=staging

Pass-through env files
----------------------
Each profile file can contain **any number of** ``KEY=VALUE`` lines.

.. code-block:: dotenv

   # .passthru.dev.env
   API_BASE_URL=http://localhost:8080
   FEATURE_FLAG_X=1
   LOG_LEVEL=debug
   MULTI_LINE_JSON={"k":"v","note":"use \\n if needed"}

Notes:
- Format: **one** ``KEY=VALUE`` per line. No quotes, no ``export``.
- Literal newlines are **not** supported inside ``env_file`` values. Use
  escaped ``\n`` and have your app decode if needed, or move multiline content
  into the YAML under ``environment:`` with a block scalar.

Bring it up
-----------
.. code-block:: bash

   export DEV_CONTAINER_PROFILE=dev
   podman compose up -d        # or docker compose up -d

Fail-fast behaviors
-------------------
- If ``DEV_CONTAINER_PROFILE`` is **missing or empty**, Compose fails with your
  message from ``:?…``.
- If the resolved file (e.g., ``.passthru.dev.env``) **does not exist**,
  Compose fails with “file not found.”

Verifying the effective environment
-----------------------------------
Show the fully rendered config (useful for confirming the resolved ``env_file`` path):

.. code-block:: bash

   podman compose config        # or docker compose config

Confirm what a plain ``exec`` will see:

.. code-block:: bash

   CTR=$(podman ps --filter name=app -q)
   podman inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$CTR" | sort
   podman exec "$CTR" env | sort

Updating variables
------------------
When you change any ``.passthru.<profile>.env`` content, **recreate** the container:

.. code-block:: bash

   # re-read the file into the container config
   podman compose up -d --force-recreate app

(Just restarting won’t apply env changes.)

Multiline values (if truly required)
------------------------------------
``env_file`` doesn’t support literal newlines. Use one of:

1) YAML block scalar (applies at create time, survives ``exec``):

.. code-block:: yaml

   services:
     app:
       environment:
         MULTI_BLOCK: |
           line 1
           line 2

2) Escaped newlines in the file and decode in app logic:

.. code-block:: dotenv

   MULTI_ESCAPED=line 1\nline 2

Security & portability tips
---------------------------
- Keep ``.passthru.*.env`` out of version control (add to ``.gitignore``) if
  they hold secrets.
- Prefer **profile-specific files** over many individual Compose edits.
- Treat values in ``env_file`` as **literals**; don’t rely on interpolation
  inside those files for portability between Docker Compose and Podman Compose.

Troubleshooting
---------------
- **“variable required” error**: Set ``DEV_CONTAINER_PROFILE`` or remove the
  ``:?…`` guard while testing.
- **Changes not visible after edit**: You forgot ``--force-recreate``.
- **Shell ``exec`` is missing env**: Ensure the keys appear in
  ``podman inspect … .Config.Env``. If not, your file wasn’t loaded or you
  didn’t recreate.

Quick checklist
---------------
- [ ] Define ``env_file`` with a **templated path** and a **required guard**.
- [ ] Provide per-profile ``.passthru.<profile>.env`` files with ``KEY=VALUE`` lines.
- [ ] Set ``DEV_CONTAINER_PROFILE`` before ``compose up``.
- [ ] Use ``--force-recreate`` after any changes.
- [ ] Verify with ``compose config``, ``inspect``, and ``exec env``.
