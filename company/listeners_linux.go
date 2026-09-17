// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build linux

package company

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

func listenerWatchSupported() bool { return true }

// listeningPorts reports the TCP ports the process at pid is listening on,
// as seen from its own network namespace.
//
// /proc/<pid>/net/tcp is the socket table of the namespace pid lives in.
// Under bubblewrap that namespace is the app's own, freshly unshared, so any
// LISTEN row in it is a port the app opened — there is nothing else in there
// to open one. Without the namespace the same file is the whole host's table
// and this answer means nothing, which is why the caller checks
// ListenerWatch before believing it.
func listeningPorts(pid int) []int { return listeningPortsIn("/proc/" + strconv.Itoa(pid) + "/net") }

// listeningPortsIn reads the tables under dir — split from the pid lookup so
// the parser can be tested against a captured table without a live process.
func listeningPortsIn(dir string) []int {
	var ports []int
	for _, name := range []string{"tcp", "tcp6"} {
		f, err := os.Open(dir + "/" + name)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Scan() // header
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			// sl local_address rem_address st ...; st 0A is LISTEN.
			if len(fields) < 4 || fields[3] != "0A" {
				continue
			}
			if i := strings.LastIndexByte(fields[1], ':'); i >= 0 {
				if port, err := strconv.ParseInt(fields[1][i+1:], 16, 32); err == nil {
					ports = append(ports, int(port))
				}
			}
		}
		_ = f.Close()
	}
	return ports
}
