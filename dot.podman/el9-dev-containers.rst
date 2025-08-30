================================================
EL9 Dev Container on WSL with Podman + Compose
================================================

This guide describes a **Podman-centric** workflow on **WSL (EL9)** that works
for two audiences:

* **VS Code users** (Windows VS Code + Remote – WSL + Dev Containers)
* **Non-VS Code users** (using ``podman-compose`` directly)

It assumes:

* Your **repo** contains ``.devcontainer/`` and ``docker-compose.yml``
  (or ``compose.yaml``).
* That repo is mounted at ``/workspace`` inside the container(s).
* The **parent directory** (holding sibling repos) is mounted at
  ``/PROJECT_ROOT`` inside the container(s).

``podman-docker`` is only sprinkled in where the Dev Containers extension
expects a Docker CLI/socket. Everything else stays Podman-native.

.. contents::
   :local:
   :depth: 2

Prerequisites (WSL EL9 distro)
------------------------------

Install Podman and Podman-Compose:

.. code-block:: bash

   sudo dnf install -y podman podman-compose
   # Optional shim for tools that expect "docker" (e.g. VS Code Dev Containers)
   sudo dnf install -y podman-docker

Enable the Podman user socket:

.. code-block:: bash

   systemctl --user enable --now podman.socket
   echo 'export DOCKER_HOST=unix:///run/user/$(id -u)/podman/podman.sock' >> ~/.bashrc
   source ~/.bashrc

Sanity checks:

.. code-block:: bash

   podman --version
   podman ps
   podman-compose version
   # If using podman-docker:
   docker ps

Repository Layout & Mounts
--------------------------

* The active repo with ``.devcontainer/`` and ``docker-compose.yml`` maps to
  ``/workspace`` inside containers.
* Its parent directory mounts at ``/PROJECT_ROOT`` for sibling repos.

Minimal ``docker-compose.yml`` (Podman-Compose)
-----------------------------------------------

Example file at the root of your repo:

.. code-block:: yaml

   version: "3.9"
   services:
     dev:
       image: your-registry/your-el9-build:latest
       container_name: el9-dev
       init: true
       userns_mode: keep-id
       ipc: private
       pid: private
       volumes:
         - .:/workspace:Z
         - ..:/PROJECT_ROOT:Z
       environment:
         LANG: C.UTF-8
         LC_ALL: C.UTF-8
       command: sleep infinity

Notes:

* ``:Z`` relabels for SELinux compatibility; drop if not needed.
* ``command: sleep infinity`` keeps the container running for attach/exec.
* Add ports, networks, or extra services as needed.

Minimal ``.devcontainer/devcontainer.json``
-------------------------------------------

.. code-block:: json

   {
     "name": "EL9 Build (Compose)",
     "dockerComposeFile": "docker-compose.yml",
     "service": "dev",
     "workspaceFolder": "/workspace",

     "customizations": {
       "vscode": {
         "settings": {
           "dev.containers.dockerPath": "podman"
         },
         "extensions": [
           "ms-vscode.cpptools",
           "ms-python.python"
         ]
       }
     }
   }

Notes:

* If you prefer the Docker CLI shim (``podman-docker``), set
  ``"dev.containers.dockerPath": "docker"``.
* VS Code will use the Compose project definition from
  ``docker-compose.yml`` and attach to the ``dev`` service.

For VS Code Users
-----------------

One-time setup (Windows side):

1. Install extensions:
   * **Remote – WSL**
   * **Dev Containers**
2. In WSL, ensure Podman socket is running and ``DOCKER_HOST`` is exported.

Daily workflow:

1. Open the repo folder (containing ``.devcontainer`` and ``docker-compose.yml``) in WSL.
2. Press ``F1`` → **Dev Containers: Reopen in Container**.
3. VS Code will bring up the Compose project with Podman and attach to the
   ``dev`` service at ``/workspace``.
4. Sibling repos are visible under ``/PROJECT_ROOT``.

Optional:

* You can also use **Attach to Running Container** or **Attach to Running
  Compose Service** if you started it manually with ``podman-compose up``.

For Non-VS Code Users (pure Podman-Compose)
-------------------------------------------

Bring up the development container(s):

.. code-block:: bash

   podman-compose up -d

Check status:

.. code-block:: bash

   podman-compose ps

Open a shell into the dev service:

.. code-block:: bash

   podman-compose exec dev bash

Stop containers:

.. code-block:: bash

   podman-compose down

Tips:

* If your current directory is the repo root (with ``docker-compose.yml``),
  volumes will map correctly:
  * Repo → ``/workspace``
  * Parent dir → ``/PROJECT_ROOT``
* Ownership matches host UID/GID if you keep ``userns_mode: keep-id``.

Troubleshooting & Sanity Checks
-------------------------------

* **Podman socket**

  .. code-block:: bash

     systemctl --user status podman.socket
     ss -lx | grep podman.sock

* **VS Code Dev Containers fails to connect**

  * Reopen VS Code WSL window after exporting ``DOCKER_HOST``.
  * If no ``podman-docker``, ensure ``"dev.containers.dockerPath": "podman"``.
  * If using ``podman-docker``, set ``"dev.containers.dockerPath": "docker"``.

* **File ownership inside containers**

  * Keep ``userns_mode: keep-id`` in Compose file.
  * Or configure ``user: "${UID}:${GID}"``.

* **Networking differences**

  * Rootless Podman uses user-mode networking.
  * Add ``network_mode: host`` if host networking is required.
  * Map ports explicitly with ``ports: - "8080:8080"``.

CI (Bamboo) Note
----------------

You can run ``podman-compose up --build`` in CI exactly like developers do.
If Bamboo agents run in Linux/WSL, Podman behaves the same way. For parity
with VS Code users, keep the Compose file and Dev Container config checked in.

Quick Checklist
---------------

* [ ] ``podman`` and ``podman-compose`` installed
* [ ] Podman user socket active; ``DOCKER_HOST`` exported
* [ ] ``docker-compose.yml`` present (with mounts to ``/workspace`` and ``/PROJECT_ROOT``)
* [ ] ``.devcontainer/devcontainer.json`` present for VS Code users
* [ ] VS Code users can **Reopen in Container** from WSL
* [ ] CLI users can ``podman-compose exec dev bash`` successfully
