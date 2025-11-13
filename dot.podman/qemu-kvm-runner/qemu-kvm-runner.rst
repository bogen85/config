===============================================
Rocky 9.6 QEMU Task Runner (Cloud-Init Runner)
===============================================

This document describes a simple pattern for using a Rocky 9.6
GenericCloud qcow2 image as a *task runner*:

* Use a small NoCloud seed ISO to inject a cloud-init task.
* Run the VM headless with QEMU in TCG mode.
* See live task output on your terminal.
* Reuse the same qcow2 so package updates persist across runs.
* Regenerate the seed ISO each run so the task always executes.

Be aware: the qcow2 image is **mutable**. Package updates and other
state changes persist across runs, which is usually what you want for
a long-lived runner VM, but it means you should treat it as an evolving
environment rather than an immutable template.


Prerequisites
=============

On the host (or inside the container where you run QEMU), install:

.. code-block:: bash

   dnf install -y qemu-kvm-core xorrisofs

You also need the Rocky 9.6 GenericCloud image:

.. code-block:: bash

   curl -O https://download.rockylinux.org/pub/rocky/9/images/x86_64/Rocky-9-GenericCloud.latest.x86_64.qcow2


Seed ISO Generator Script (make-seed.sh)
========================================

The following script creates a NoCloud seed ISO which:

* Writes a marker file ``/root/cloud-init-ran`` (proving root access).
* Logs everything to the serial console (visible via ``-nographic``).
* Attempts an early ``umount /boot`` to reduce shutdown noise.
* Uses the **seconds since epoch** as ``instance-id`` so the task runs
  on every run (fresh instance from cloud-init’s perspective).
* Lets cloud-init perform a clean shutdown via ``power_state`` (no
  traceback spam).

Save this as ``make-seed.sh`` and make it executable.

.. code-block:: bash

   #!/bin/bash
   set -euo pipefail

   SEED_DIR=seed
   ISO=seed.iso

   rm -rf "$SEED_DIR" "$ISO"
   mkdir "$SEED_DIR"

   # Generate a unique instance-id using seconds since epoch
   INSTANCE_ID=$(date +%s)

   echo "Using instance-id: $INSTANCE_ID"

   ########################################
   # user-data: run a simple root task and log to serial
   ########################################
   cat > "$SEED_DIR/user-data" << EOF
   #cloud-config
   write_files:
     - path: /root/run-task.sh
       permissions: '0755'
       content: |
         #!/bin/bash
         # Send ALL output to the QEMU serial console
         exec > /dev/ttyS0 2>&1

         echo "============================================"
         echo "   [run-task.sh] Minimal root test"
         echo "============================================"
         date

         echo "[*] Doing a root-only operation..."
         echo "cloud-init ran as root at \$(date)" > /root/cloud-init-ran
         ls -l /root/cloud-init-ran

         echo "[*] Attempting early umount of /boot..."
         umount /boot >/dev/null 2>&1 || true

         echo "============================================"
         echo " [run-task.sh] Task complete (no poweroff)"
         echo "============================================"

   runcmd:
     - [ /root/run-task.sh ]

   # Let cloud-init shut the system down cleanly AFTER all stages
   power_state:
     mode: poweroff
     message: "cloud-init finished, powering off"
     timeout: 30
     condition: True
   EOF

   ########################################
   # meta-data with dynamic instance-id
   ########################################
   cat > "$SEED_DIR/meta-data" << EOF
   instance-id: $INSTANCE_ID
   local-hostname: test-1
   EOF

   ########################################
   # Build the NoCloud seed ISO
   ########################################
   xorrisofs \
     -output "$ISO" \
     -volid cidata \
     -joliet -rock \
     "$SEED_DIR/user-data" "$SEED_DIR/meta-data"

   echo "Created $ISO (instance-id $INSTANCE_ID)"


Running the Task Runner VM
==========================

Each time you want to run a new job:

1. Regenerate the seed ISO (new instance-id):

   .. code-block:: bash

      ./make-seed.sh

2. Launch QEMU with the Rocky qcow2 and the seed ISO:

   .. code-block:: bash

      /usr/libexec/qemu-kvm \
        -cpu max \
        -machine accel=tcg \
        -m 4096 -smp 4 \
        -drive file=./Rocky-9-GenericCloud.latest.x86_64.qcow2,if=virtio,format=qcow2 \
        -drive file=seed.iso,if=virtio,format=raw,readonly=on \
        -nographic

With ``-nographic``, all kernel and cloud-init messages, plus the
output from ``run-task.sh``, appear directly in your terminal.

You should see something like:

.. code-block:: text

   ============================================
      [run-task.sh] Minimal root test
   ============================================
   Thu Nov 13 10:49:xx UTC 2025
   [*] Doing a root-only operation...
   -rw-r--r--. 1 root root 44 Nov 13 10:49 /root/cloud-init-ran
   [*] Attempting early umount of /boot...
   ============================================
    [run-task.sh] Task complete (no poweroff)
   ============================================
   cloud-init finished, powering off
   ...
   (clean shutdown messages)

Because the qcow2 is reused, any package updates you perform inside
the runner VM (for example, when you later add real build tasks in
place of the minimal root test) will persist across runs. The changing
``instance-id`` in the seed ISO ensures cloud-init still runs your task
each time, even though the underlying disk is the same.


Notes on Mutable State
======================

* The qcow2 image is **not** immutable. Over time it accumulates:
  - package updates,
  - cache files,
  - any changes your tasks make to the system.
* If you ever want a clean slate again, copy or re-download the
  GenericCloud image and start over from that template.
* For a long-lived “builder VM” used as a task runner, this mutable
  behavior is often desirable: you only pay the update cost once, but
  still get fresh cloud-init tasks on each run via the changing
  ``instance-id``.
