var (
	nameFlag    string
	overlayFlag bool
	initFlag    bool
	runHostFlag bool
	showCmdFlag bool
	volumes     multiFlag // NEW
)

flag.StringVar(&nameFlag, "name", "", "Container name (default: derived from ROOTFS)")
flag.BoolVar(&showCmdFlag, "showcmd", false, "Show podman command before executing it")
flag.BoolVar(&overlayFlag, "overlay", false, "Use writable overlay (:O) on ROOTFS")
flag.BoolVar(&runHostFlag, "runhost", false, "bind host / to container /run/host")
flag.BoolVar(&initFlag, "init", false, "Run a full init inside the container")

// Repeatable --volume; also provide -v as a shorthand alias.
flag.Var(&volumes, "volume", "Bind mount (may be repeated). Same format as podman: SRC:DST[:OPTIONS]")
flag.Var(&volumes, "v",      "Alias for --volume (may be repeated)")
