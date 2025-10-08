.. code-block:: text

   Short version: there’s no built-in “devcontainer profile” dropdown that appears every time you reopen.
   But you can get a clean picker workflow using one of these supported patterns:

   ==========================================================
   1) Multiple devcontainer configs (native picker)
   ==========================================================
   - Format your tool’s output as multiple `.devcontainer/<profile>/devcontainer.json` (and companion files).
   - When the folder has more than one devcontainer config, VS Code shows a QuickPick to choose which config to open.
   - Great when profiles are “whole config” variants.

   Example tree (your tool writes these):
   -------------------------------------
   .. code-block:: text

      .devcontainer/
        dev/            # profile name
          devcontainer.json
          compose.yml
          .env           # optional
        ci/
          devcontainer.json
          compose.yml
          .env
        prod/
          devcontainer.json
          compose.yml

   Open command: “Dev Containers: Reopen in Container” → VS Code lists `dev`, `ci`, `prod`.

   ==========================================================
   2) Compose profiles + per-profile `.env` files (picker via VS Code Tasks)
   ==========================================================
   - Output one Compose file that uses Compose profiles, plus per-profile `.env.<name>` files.
   - Add a VS Code Task with an `inputs.quickPick` to choose a profile; the task copies/symlinks the chosen `.env.<name>` to `.devcontainer/.env`
     and sets `COMPOSE_PROFILES=<name>`.
   - User runs “Tasks: Run Task → Select Dev Profile”, then “Dev Containers: Rebuild”.

   What your tool outputs:
   -----------------------
   .. code-block:: text

      .devcontainer/
        compose.yml          # uses 'profiles:' keys
        .env.dev
        .env.ci
        .env.prod

   Compose snippet:
   ----------------
   .. code-block:: yaml

      services:
        app:
          profiles: ["${COMPOSE_PROFILES:-dev}"]
          image: ${REGISTRY}/myapp:${IMAGE_TAG}

   Minimal `.vscode/tasks.json` (picker):
   -------------------------------------
   .. code-block:: json

      {
        "version": "2.0.0",
        "inputs": [
          {
            "id": "devProfile",
            "type": "pickString",
            "description": "Select dev profile",
            "options": ["dev","ci","prod"],
            "default": "dev"
          }
        ],
        "tasks": [
          {
            "label": "Select Dev Profile",
            "type": "shell",
            "command": "cp .devcontainer/.env.${input:devProfile} .devcontainer/.env && echo COMPOSE_PROFILES=${input:devProfile} > .devcontainer/.compose_profiles",
            "problemMatcher": []
          }
        ]
      }

   Then in `devcontainer.json`:
   ----------------------------
   .. code-block:: json

      {
        "name": "my-dev",
        "dockerComposeFile": [".devcontainer/compose.yml"],
        "service": "app",
        "initializeCommand": "test -f .devcontainer/.compose_profiles && export $(cat .devcontainer/.compose_profiles) ; true"
      }

   (Compose itself reads `.devcontainer/.env`; `COMPOSE_PROFILES` can be exported similarly.)

   ==========================================================
   3) Feature option (dropdown during config generation)
   ==========================================================
   - Package your choices as a Dev Container “Feature” with an enum option. VS Code shows a dropdown when the feature is added;
     the choice is written into `devcontainer.json`.
   - Your tool then generates `.env`/Compose fragments from that value.
   - This is a one-time selection (at config authoring), not a per-reopen picker.

   Feature manifest your tool can emit:
   ------------------------------------
   .. code-block:: json

      {
        "name": "Profile Selector",
        "id": "profile-selector",
        "version": "1.0.0",
        "options": {
          "profile": {
            "type": "string",
            "enum": ["dev","ci","prod"],
            "default": "dev",
            "description": "Select profile"
          }
        }
      }

   Then `devcontainer.json` will contain:
   --------------------------------------
   .. code-block:: json

      {
        "features": {
          "ghcr.io/you/profile-selector:1": { "profile": "ci" }
        }
      }

   Your init script reads that value and writes `.env`.

   ==========================================================
   4) Multiple Compose override files (picker via tasks)
   ==========================================================
   - Output `compose.yml` plus `compose.override.dev.yml`, `compose.override.ci.yml`, etc., and `.env.<profile>`.
   - A VS Code task with a QuickPick writes a small `.compose-files` list that `devcontainer.json` references via `${localWorkspaceFolder}`
     to choose which files to pass.
   - Works well if each profile needs structural YAML changes.

   What to output:
   ----------------
   .. code-block:: text

      .devcontainer/
        compose.yml
        compose.override.dev.yml
        compose.override.ci.yml
        compose.override.prod.yml
        .env.dev
        .env.ci
        .env.prod

   In `devcontainer.json`:
   -----------------------
   .. code-block:: json

      {
        "dockerComposeFile": [
          ".devcontainer/compose.yml",
          "${localEnv:DEV_OVERRIDE_FILE-.devcontainer/compose.override.dev.yml}"
        ],
        "initializeCommand": "cp .devcontainer/.env.${localEnv:DEV_PROFILE-dev} .devcontainer/.env"
      }

   A task sets `DEV_PROFILE` and `DEV_OVERRIDE_FILE` via a QuickPick before rebuild.

   ==========================================================
   Recommended output formats for your generator
   ==========================================================
   * Per-profile `.env.<name>` files (simple, Compose-native).
   * Optional per-profile `compose.override.<name>.yml` when you need structural diffs.
   * Or full per-profile `.devcontainer/<name>/` folders if you want the built-in VS Code QuickPick with zero extra wiring.

   ==========================================================
   TL;DR
   ==========================================================
   If you want the cleanest UX with a native picker and zero scripting, generate **multiple `.devcontainer/<profile>/devcontainer.json` folders**.
   If you prefer a single config with a lightweight chooser, generate **`.env.<profile>` (+ optional overrides)** and add a **VS Code task QuickPick**
   that updates `.devcontainer/.env` (and `COMPOSE_PROFILES`) before rebuilding.
