
=============================================================
Zombie Provenance *Lite*: Logging Orphans from Busy Parents
=============================================================

Target: EL9 container PID 1 supervisor (Go).
Scope: No ``CN_PROC`` / connector, no full fork tracking.
Goal: Log useful provenance for zombies whose parents are alive-but-busy, and
later correlate when they get adopted/reaped by the supervisor.

Why this document
-----------------
Sometimes a parent process exits children cleanly. Sometimes it doesn't.
You already reap orphans as PID 1. This guide lets you *also* log
**foreign zombies** (children whose parents are still alive but not waiting)
*before* they are adopted by you—capturing their *true parent* and other
clues—then correlate that information when you eventually reap them.

Terms
-----
- **Zombie (``Z``)**: A child that has exited and awaits its parent's
  ``wait*()``. It still has a ``/proc/<pid>`` entry; the child’s own
  ``cmdline`` is empty at this point.
- **Orphan**: A process whose parent has exited. Orphans are reparented to
  the nearest subreaper or PID 1.
- **Adoption**: The kernel’s act of reparenting a process (including a zombie)
  to your supervisor because you are the subreaper/PID 1.
- **Foreign zombie**: A zombie whose parent PID (``PPid`` in ``/proc/<pid>/status``)
  is *not* your supervisor (i.e., the real parent is still alive).

Key observations
----------------
1. While a parent is *alive and has not waited*, the zombie’s ``PPid`` in
   ``/proc/<pid>/status`` is the **true parent**. That’s your best moment to
   snapshot the parent’s identity and command line.
2. When the parent later exits without waiting, the zombie is adopted by you;
   its ``PPid`` changes to your PID and you will reap it in your normal
   ``waitid``/``waitpid`` loop.
3. Process IDs can be reused; pair ``pid`` with the child’s ``starttime``
   (``/proc/<pid>/stat`` field 22) to identify tasks robustly until reboot.

Design overview
---------------
- Run a periodic **zombie sweep** (e.g., every 250–1000 ms).
- For each task with ``State: Z`` and ``PPid != myPID``, store a **cache entry**::

    key:   { child_pid, child_starttime }
    value: {
      child_comm,
      first_seen_zombie_ts,
      parent_pid,
      parent_starttime,
      parent_comm,
      parent_cmdline_snapshot
    }

- Keep a TTL/LRU (e.g., 5–10 minutes) to bound memory.
- On every reap in your SIGCHLD-driven loop, look up the cache by
  ``{pid,starttime}`` and enrich the exit log.

What to capture
---------------
- **Child**: ``pid``, ``starttime`` (stat#22), ``comm`` (short name), first-seen timestamp.
- **Parent (true origin)**: ``PPid`` (from child’s status), parent’s ``starttime``
  (``/proc/<ppid>/stat`` #22), parent’s ``comm`` and **full ``cmdline``**
  (from ``/proc/<ppid>/cmdline``).

Scanner algorithm
-----------------
1. Enumerate ``/proc`` entries that are numeric.
2. For each PID:
   - Read ``/proc/<pid>/status``.
   - If ``State: Z`` and ``PPid != myPID``:
     - Read child ``/proc/<pid>/stat``; parse field 22 → ``child_starttime``.
     - Read parent details from ``/proc/<ppid>`` (``stat`` #22, ``status`` Name,
       ``cmdline`` full argv).
     - Insert/update cache entry keyed by ``{pid, child_starttime}``.
     - (Optional) If parent looks wedged (e.g., ``D`` state for long), annotate.
3. Age out old entries (TTL) and purge on observed reaps.

Reaper integration
------------------
- In your ``waitid``/``waitpid`` loop:
  - Resolve the reaped child’s ``starttime`` *while it is (briefly) zombie* by
    reading ``/proc/<pid>/stat`` (still available at reap time).
  - Lookup cache by ``{pid,starttime}``.
  - If present, log with **origin details** and compute durations:
    - ``zombie_duration`` (now – first_seen_zombie_ts)
    - (If adopted earlier) ``under_my_care`` duration (from adoption to reap).
  - If absent, fall back to your current minimal logging.

Recommended log lines
---------------------
- When first seen during scan::

    [foreign-zombie] pid=%d ppid=%d child_comm=%q parent_comm=%q parent_cmd=%q \
    child_start_jiffies=%d parent_start_jiffies=%d

- On reap (enriched)::

    [reap] pid=%d rc=%d sig=%d child_comm=%q orphaned_by_ppid=%d \
    parent_start_jiffies=%d zombie_for=%s under_my_care=%s

Intervals & performance
-----------------------
- In a container PID namespace, ``/proc`` is small; a 1 Hz scan is usually sub-ms.
- Start with 500–1000 ms; drop to 250 ms during “busy” periods if you care about
  very short windows between “became zombie” and “parent died.”

Edge cases & races
------------------
- A zombie may be created and adopted + reaped between scans. You’ll still see
  the reap, but you won’t have captured the foreign-parent info. If this matters,
  trigger an extra scan whenever you reap *any* child.
- ``cmdline`` for the **child** is empty when in ``Z``. Always snapshot the **parent**.
- Handle permission errors gracefully (rare inside container namespaces).

Configuration knobs
-------------------
- Scan interval (ms).
- Cache TTL (minutes).
- Maximum cache entries (LRU cap).
- Optional trigger: run an immediate scan whenever a child is reaped.

Minimal Go helper (drop-in snippets)
------------------------------------

.. code-block:: go

    // types you might add next to your supervisor.go
    type ZKey struct {
        PID       int
        Starttime uint64 // child /proc/<pid>/stat field 22
    }
    type ZMeta struct {
        ChildComm   string
        FirstSeen   time.Time
        ParentPID   int
        ParentST    uint64
        ParentComm  string
        ParentCmd   string
    }

    var (
        myPID     = os.Getpid()
        zcache    = map[ZKey]ZMeta{}
        zcacheTTL = 10 * time.Minute
        mu        sync.Mutex
    )

    func childStarttime(pid int) (uint64, error) { /* parse /proc/<pid>/stat; field 22 */ }
    func readStatus(pid int) (ppid int, comm string, state byte, err error) { /* parse /proc/<pid>/status */ }
    func readCmdline(pid int) string { /* read /proc/<pid>/cmdline; join with spaces */ }

    func scanForeignZombies(now time.Time) {
        entries, _ := os.ReadDir("/proc")
        mu.Lock()
        defer mu.Unlock()

        for _, e := range entries {
            pid, err := strconv.Atoi(e.Name())
            if err != nil { continue }
            ppid, childComm, state, err := readStatus(pid)
            if err != nil || state != 'Z' || ppid == myPID { continue }

            cst, err := childStarttime(pid)
            if err != nil { continue }

            pst, pcomm := uint64(0), ""
            if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", ppid)); err == nil {
                // parse parent starttime (field 22) similar to child
                pst = /* ... */
            }
            pcomm = func() string {
                if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", ppid)); err == nil {
                    for _, ln := range bytes.Split(b, []byte{'\n'}) {
                        if bytes.HasPrefix(ln, []byte("Name:\t")) { return string(ln[6:]) }
                    }
                }
                return ""
            }()
            pcmd := readCmdline(ppid)

            key := ZKey{PID: pid, Starttime: cst}
            if _, exists := zcache[key]; !exists {
                zcache[key] = ZMeta{
                    ChildComm:  childComm,
                    FirstSeen:  now,
                    ParentPID:  ppid,
                    ParentST:   pst,
                    ParentComm: pcomm,
                    ParentCmd:  pcmd,
                }
                log.Printf("[foreign-zombie] pid=%d ppid=%d child_comm=%q parent_comm=%q parent_cmd=%q child_start_jiffies=%d parent_start_jiffies=%d",
                    pid, ppid, childComm, pcomm, pcmd, cst, pst)
            } else {
                // refresh if you want; otherwise leave as first-seen snapshot
            }
        }

        // TTL sweep
        cutoff := now.Add(-zcacheTTL)
        for k, v := range zcache {
            if v.FirstSeen.Before(cutoff) {
                delete(zcache, k)
            }
        }
    }

    // Reaper hook: call this inside your SIGCHLD loop when you reap a child.
    func enrichReapLog(pid int, status syscall.WaitStatus) {
        cst, err := childStarttime(pid)
        if err != nil { // log minimal, we couldn't enrich
            log.Printf("[reap] pid=%d rc=%d sig=%d", pid, status.ExitStatus(), status.Signal())
            return
        }
        key := ZKey{PID: pid, Starttime: cst}
        mu.Lock()
        meta, ok := zcache[key]
        if ok {
            delete(zcache, key) // done with it
        }
        mu.Unlock()

        if ok {
            lived := time.Since(meta.FirstSeen)
            log.Printf("[reap] pid=%d rc=%d sig=%d child_comm=%q orphaned_by_ppid=%d parent_start_jiffies=%d zombie_for=%s",
                pid, status.ExitStatus(), status.Signal(), meta.ChildComm, meta.ParentPID, meta.ParentST, lived)
        } else {
            log.Printf("[reap] pid=%d rc=%d sig=%d", pid, status.ExitStatus(), status.Signal())
        }
    }

Operational advice
------------------
- Start the scanner with a ticker (e.g., 500–1000 ms). Consider a burst-mode
  faster interval if you see many concurrent exits.
- Always guard cache updates with a mutex; scans and reaps can interleave.
- Prefer ``waitid`` over ``waitpid`` for richer status and non-blocking patterns.

Testing recipe
--------------
1. In another shell inside the container, run a simple zombie-maker (parent
   never waits)::

      # Python one-liner
      python3 - <<'PY'
      import os, time
      while True:
          pid = os.fork()
          if pid == 0:
              os._exit(0)
          time.sleep(0.2)   # tune rate
      PY

2. Your supervisor should log ``[foreign-zombie]`` entries with the true ``PPid``
   and parent cmdline while the parent is alive.
3. Kill the parent. Watch your supervisor adopt and then log the enriched
   ``[reap]`` line with ``orphaned_by_ppid=...`` and the zombie duration.

Future upgrades
---------------
- Add an "immediate scan on reap" trigger to reduce misses during tight races.
- If you later enable ``CONFIG_CONNECTOR`` + ``CONFIG_PROC_EVENTS``:
  *Subscribe to FORK/EXEC/EXIT* and build a small origin map to capture
  provenance even earlier.

Appendix: /proc fields used
---------------------------
- ``/proc/<pid>/status``: ``Name:``, ``State:``, ``PPid:``
- ``/proc/<pid>/stat``: field 22 = starttime (jiffies since boot)
- ``/proc/<pid>/cmdline``: NUL-separated argv (empty for zombies)

License
-------
This document and the included code snippets are provided under the MIT license.
