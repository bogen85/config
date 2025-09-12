#/usr/bin/env bash

set -euo pipefail

p=podman
t0=bb$$:base0
t1=bb$$:inst1

bb=$(which busybox)
tar cf - "$bb"|$p import - $t0
c=$(
$p run --rm -d $t0 "$bb" sh -c "
	$bb mv /usr/sbin /bin;
	/bin/busybox --install -s /bin
	sleep 0.6
")
$p commit --change 'CMD ["/bin/sh","-l"]' "$c" $t1
$p rm -f "$c" || true
$p run -it --rm $t1 || echo "(rc=$?)"
$p rmi -f $t0
$p rmi -f $t1
