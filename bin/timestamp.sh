#!/usr/bin/env bash

set -euo pipefail

# Get the current epoch time in seconds
epoch_seconds=$(date +%s)

# Calculate the modulus for 100 years (200 * 365.25 * 24 * 60 * 60)
modulus=$((100 * 365 * 24 * 60 * 60))

# Apply the modulus
mod_epoch=$((epoch_seconds % modulus))

# Get the current milliseconds
milliseconds=$(date +%3N)

# Combine the results
formatted_timestamp="${mod_epoch}.${milliseconds}"

# Output the result
echo -n "$formatted_timestamp"
