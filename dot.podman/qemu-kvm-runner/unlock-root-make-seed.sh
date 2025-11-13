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
cat > "$SEED_DIR/user-data" << 'EOF'
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
      echo "cloud-init ran as root at $(date)" > /root/cloud-init-ran
      ls -l /root/cloud-init-ran

      echo "[*] Unlocking root account and clearing password (INSECURE DEV ONLY)..."
      # Remove root's password hash and unlock the account
      passwd -d root || true
      passwd -u root || true

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
