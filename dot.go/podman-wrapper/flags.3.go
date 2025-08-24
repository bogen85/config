// ... existing pargs setup ...

if runHostFlag {
	pargs = append(pargs,
		"--volume", "/:/run/host:ro,rslave",
		"--volume", "/mnt:/run/host/mnt:rw,rslave",
		"--volume", "/home:/run/host/home:rw,rslave",
	)
}

// Bind the rootfs journal as you already do...
journalHost := filepath.Join(rootfs, "var", "log", "journal")
ensureDirHost(journalHost, 0o755)
pargs = append(pargs, "-v", journalHost+":/var/log/journal:Z")
if showCmdFlag {
	fmt.Fprintf(os.Stderr, "journald bind: %s -> /var/log/journal\n", journalHost)
}

// Append user-provided volumes (repeatable)
for _, v := range volumes {
	pargs = append(pargs, "--volume", v)
}

// LAST option must be --rootfs
pargs = append(pargs, "--rootfs", rootfsArg)
pargs = append(pargs, cmdSlice...)
