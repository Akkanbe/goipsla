//declscope:namespace socket

package api

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

// This file sets up the Unix domain socket of the Server: its lock, its
// group and the removal of a stale socket file.

// socketProbeTimeout bounds the "is another daemon listening" check.
const socketProbeTimeout = time.Second

// lockSocket takes an exclusive, non-blocking flock on "<path>.lock". The
// lock file is not removed afterwards: removing it would let a third process
// lock a new file while the second still holds the old one.
//
//declscope:package // NewServer sets up its socket with it
func lockSocket(path string) (*os.File, error) {
	name := path + ".lock"
	f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", name, err)
	}
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("%s is locked by another process (is goipslad already running?)", name)
		}
		return nil, fmt.Errorf("lock %s: %w", name, err)
	}
	return f, nil
}

// lookupSocketGroup resolves the socket group, a name or a numeric gid.
//
//declscope:package // NewServer sets up its socket with it
func lookupSocketGroup(name string) (int, error) {
	g, err := user.LookupGroup(name)
	if err != nil {
		if _, numErr := strconv.Atoi(name); numErr == nil {
			g, err = user.LookupGroupId(name)
		}
	}
	if err != nil {
		return 0, fmt.Errorf("socket group %q: %w", name, err)
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return 0, fmt.Errorf("socket group %q: invalid gid %q", name, g.Gid)
	}
	return gid, nil
}

// removeStaleSocket removes path if it is a socket that refuses
// connections (ECONNREFUSED: nobody listens). The caller holds the lock.
//
//declscope:package // NewServer sets up its socket with it
func removeStaleSocket(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a socket", path)
	}
	conn, err := net.DialTimeout("unix", path, socketProbeTimeout)
	switch {
	case err == nil:
		conn.Close()
		return fmt.Errorf("another process is listening on %s (is goipslad already running?)", path)
	case errors.Is(err, unix.ECONNREFUSED):
		// stale: remove below
	case errors.Is(err, unix.ENOENT):
		return nil // gone meanwhile
	default:
		return fmt.Errorf("cannot tell whether %s is in use, not removing it: %w", path, err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale socket %s: %w", path, err)
	}
	return nil
}
