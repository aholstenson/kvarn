package scheduler

import (
	"context"
	"log/slog"
	"time"
)

// defaultDiskWatchInterval is how often WatchHostDisk samples free space when
// the caller does not pick an interval. VM disks grow over minutes, not
// milliseconds, and statfs on a busy host is not free, so a coarse poll is
// enough to catch a fill before it becomes an ENOSPC mid-job.
const defaultDiskWatchInterval = 30 * time.Second

// diskGateOpenLocked reports whether admission is permitted by the host disk
// guard. It fails open in the two cases where a closed gate would be a guess
// rather than a measurement: no floor configured, and no sample taken yet.
func (s *Scheduler) diskGateOpenLocked() bool {
	if s.diskFloor == 0 || !s.diskMeasured {
		return true
	}
	return s.diskAvail >= s.diskFloor
}

// UpdateDiskAvailable records the real free space on the watched filesystem.
// Crossing the floor in either direction is a state change worth acting on:
// falling below it stops admission even though the accounting pool still has
// room, and rising back above it must wake the queue, because nothing else
// will — the capacity that frees a stuck queue here is reclaimed outside the
// scheduler (a finished VM's disk, an evicted image, an operator's cleanup),
// not by a Lease.Release.
func (s *Scheduler) UpdateDiskAvailable(avail uint64) {
	if s.unbounded || s.diskFloor == 0 {
		return
	}

	s.mu.Lock()
	was := s.diskGateOpenLocked()
	s.diskAvail = avail
	s.diskMeasured = true
	now := s.diskGateOpenLocked()

	var notes []notification
	if now {
		notes = s.tryAdmitLocked()
	} else if was {
		// Tell anyone already queued why they stopped moving.
		notes = s.collectNotificationsLocked()
	}
	queued := len(s.queue)
	s.mu.Unlock()

	fireNotifications(notes)

	switch {
	case was && !now:
		slog.Warn("host disk below reserve; admission paused",
			"path", s.diskPath, "available", avail, "floor", s.diskFloor, "queued", queued)
	case !was && now:
		slog.Info("host disk recovered; admission resumed",
			"path", s.diskPath, "available", avail, "floor", s.diskFloor, "queued", queued)
	}
}

// DiskGuard reports the guard's current view: the last measured free space on
// the watched filesystem, the floor it is held against, and whether admission
// is currently permitted. measured is false until the first sample lands.
func (s *Scheduler) DiskGuard() (avail, floor uint64, measured, open bool) {
	if s.unbounded {
		return 0, 0, false, true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.diskAvail, s.diskFloor, s.diskMeasured, s.diskGateOpenLocked()
}

// WatchHostDisk samples free space on the configured DiskPath every interval
// until ctx is done, feeding each reading to UpdateDiskAvailable. A zero
// interval uses the built-in default. It returns immediately when no guard is
// configured, so callers can start it unconditionally.
//
// The first sample is taken inside the loop rather than before it: this runs as
// a goroutine at startup, and the guard fails open until measured, so there is
// nothing to be gained by making the caller wait on a syscall.
func (s *Scheduler) WatchHostDisk(ctx context.Context, interval time.Duration) {
	if s.unbounded || s.diskPath == "" || s.diskFloor == 0 {
		return
	}
	if interval <= 0 {
		interval = defaultDiskWatchInterval
	}

	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		avail, err := HostFreeDiskBytes(s.diskPath)
		if err != nil {
			// Keep the last reading rather than closing the gate: a failing
			// statfs says nothing about how full the disk is, and stalling
			// every job on it would turn a monitoring fault into an outage.
			slog.Warn("host disk sample failed", "path", s.diskPath, "error", err)
		} else {
			s.UpdateDiskAvailable(avail)
		}

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
