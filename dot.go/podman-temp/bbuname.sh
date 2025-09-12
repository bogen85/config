#/usr/bin/env bash

set -euo pipefail

p=podman
t=bb$$:base

bb=$(which busybox)
tar cf - "$bb"|$p import - $t
$p run -t --rm $t $bb uname -a
$p rmi -f $t
