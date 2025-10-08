.. code-block:: text

   Goal
   ====
   Keep a single profile backend (TOML/JSON/etc.) and have both VS Code Dev Containers
   and Podman/Docker Compose consume it **without duplicating YAML** or generating
   per-profile files.

   What to Output From Your Tool
   =============================
   - One canonical profile source of truth (e.g., `profiles.toml` or `profiles.json`).
   - A **stable** `.devcontainer/compose.yml` that uses YAML anchors/extension fields (`x-*`)
     and `${VAR}` interpolation everywhere (no hardcoded values).
   - Optionally: create/update **exactly one** stable `.devcontainer/.env` *or* a symlink
     that always points to the active profile env file. (This preserves DRY while avoiding
     YAML duplication.)

   DRY Compose Structure (Anchors + Env Interpolation)
   ===================================================
   Use anchors to avoid copy/paste in Compose. All knobs come from env:

   .. code-block:: yaml

      x-base: &base
        image: ${REGISTRY:-registry.example.com}/${IMAGE_NAME:-myapp}:${IMAGE_TAG:-latest}
        env_file:
          - .devcontainer/.env
        environment:
          APP_MODE: ${APP_MODE:-dev}
          LOG_LEVEL: ${LOG_LEVEL:-info}
        volumes:
          - ${SRC_DIR:-.}:/workspace:cached

      services:
        app:
          <<: *base
          profiles: ["${COMPOSE_PROFILES:-dev}"]
          command: ${APP_CMD:-bash}

        worker:
          <<: *base
          profiles: ["${COMPOSE_PROFILES:-dev}"]
          command: ${WORKER_CMD:-sleep infinity}

   Notes:
   - **All** differences between profiles (tags, registry, flags, mounts) come from env.
   - Use `profiles:` + `COMPOSE_PROFILES` to enable/disable groups of services per profile.

   VS Code Wiring (No YAML Duplication)
   ====================================
   devcontainer.json references the single Compose file and a single service:

   .. code-block:: json

      {
        "name": "unified-dev",
        "dockerComposeFile": [".devcontainer/compose.yml"],
        "service": "app",
        "workspaceFolder": "/workspace",

        // Option A: read currently-selected profile from a tiny file and export to host env
        "initializeCommand": "test -f .devcontainer/.profile && export $(cat .devcontainer/.profile); ln -sf .env.${COMPOSE_PROFILES:-dev} .devcontainer/.env || true",

        // Optional: pass container-only vars after startup (not for Compose interpolation)
        "containerEnv": {
          "INSIDE_ONLY_FLAG": "1"
        },

        "overrideCommand": false
      }

   Selection UX (Zero YAML Duplication)
   ===================================
   Use a VS Code Task QuickPick to set the active profile. This **only** updates a tiny
   text file and a symlink—no file generation, no duplicate Compose:

   .. code-block:: json

      {
        "version": "2.0.0",
        "inputs": [
          {
            "id": "devProfile",
            "type": "pickString",
            "description": "Select dev profile",
            "options": ["dev", "ci", "prod"],
            "default": "dev"
          }
        ],
        "tasks": [
          {
            "label": "Select Dev Profile",
            "type": "shell",
            "command": "echo COMPOSE_PROFILES=${input:devProfile} > .devcontainer/.profile && ln -sf .env.${input:devProfile} .devcontainer/.env",
            "problemMatcher": []
          }
        ]
      }

   How Your Tool Plugs In (Single Backend, No Duplication)
   =======================================================
   - Your tool reads `profiles.toml` (or JSON) and **writes/updates** **one** `.env.<profile>`
     file per profile (or even keeps them in memory and writes the symlink target on demand).
     Example outputs (kept DRY by your single backend):
       - `.devcontainer/.env.dev`
       - `.devcontainer/.env.ci`
       - `.devcontainer/.env.prod`
   - The active profile is selected by writing:
       - `.devcontainer/.profile` (e.g., `COMPOSE_PROFILES=ci`)
       - a symlink `.devcontainer/.env -> .devcontainer/.env.ci`
   - Compose interpolation then uses **only** `.devcontainer/.env` + host env; no YAML copies.

   Optional: No Symlink Variant
   ============================
   If you’d rather avoid symlinks, have your tool copy the selected file:
   `cp .env.ci .env`. (Still DRY; you maintain **one** Compose and **one** env active at a time.)

   Optional: Compose Override Files (When Structure Must Change)
   ============================================================
   If some profiles need structural YAML diffs (extra services, different healthchecks),
   keep a **single** base and a **few** targeted overrides—still DRY:

   .. code-block:: json

      {
        "dockerComposeFile": [
          ".devcontainer/compose.yml",
          "${localEnv:DEV_OVERRIDE_FILE-.devcontainer/compose.override.dev.yml}"
        ],
        "initializeCommand": "echo DEV_OVERRIDE_FILE=.devcontainer/compose.override.${COMPOSE_PROFILES:-dev}.yml > .devcontainer/.override && ln -sf .env.${COMPOSE_PROFILES:-dev} .devcontainer/.env"
      }

   Keep It DRY—Checklist
   =====================
   - ✅ One base `compose.yml` with anchors and `${VAR}` everywhere.
   - ✅ One profile backend (TOML/JSON) that your tool reads—no pasted YAML.
   - ✅ One active `.env` (via symlink or copy) + one tiny `.profile` file for `COMPOSE_PROFILES`.
   - ✅ Optional minimal overrides only when structure truly changes.

   TL;DR
   =====
   Use a **single** Compose file with YAML anchors and env interpolation, control profiles via
   `COMPOSE_PROFILES`, and switch profiles in VS Code with a **QuickPick task** that updates:
   - `.devcontainer/.profile` (stores the chosen profile),
   - `.devcontainer/.env` (symlink or copy to the matching `.env.<profile>`).

   This keeps everything DRY: one backend for config, one Compose file, zero YAML duplication.
