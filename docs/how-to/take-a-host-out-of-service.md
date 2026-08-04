# Take a host out of service

Stopping the orchestrator kills whatever it is running. A job that dies mid-run
loses a VM, the agent's reasoning and whatever it spent getting there — and a
job past the point of pushing cannot be safely retried at all.

Draining avoids that. It stops the orchestrator dispatching new work while
leaving what is already running alone, so the host empties itself and can then
be stopped with nothing in flight to lose.

## Drain, wait, stop

```sh
kvarn queue drain --reason "deploying v1.4" --wait
sudo systemctl stop kvarn
```

`--wait` blocks until nothing is left running, printing the count as it falls,
and exits non-zero if that has not happened within `--timeout` (default 30
minutes) — so the two lines above work as a deploy script without the second one
running against a host still doing something.

Without `--wait` the command returns immediately and you can watch instead:

```sh
kvarn queue status
```

A drained host says so on the first line, along with the reason and since when.
That line matters: on a drained host every other number reads differently, and
without it a backlog that is not moving looks like a capacity problem.

## What happens to the work

**Jobs already running keep running.** They hold a VM, have spent against their
cost cap, and may have pushed a commit — none of that survives a restart, so a
drain lets them finish.

**Jobs that had not started running go back to the backlog.** A job still
cloning, waiting for capacity or booting its VM has done nothing but read, so
re-running it reaches the same place. Sending them back is what lets a host
reach empty in the time its genuine work takes rather than its whole pipeline's.
They come back on this host when you resume, or on whichever host picks them up
after a restart. Each one spends an attempt, the same as a job requeued by a
restart, and a job already at `max_attempts` is cancelled rather than requeued.

**Submissions are still accepted.** The backlog is durable and costs a row per
entry, so a job submitted to a draining host waits and then runs — here after a
resume, or elsewhere after a restart. Refusing would discard work the host is
perfectly able to hold and turn a rolling restart into an outage for everyone
submitting during it.

**Nothing in the backlog expires.** The `max_queue_wait` sweep stands down with
the rest of the dispatcher, so a long maintenance window does not fail the work
it was protecting. It resumes when dispatch does.

## Call it off

A drain is a state, not a countdown. If the deploy is cancelled:

```sh
kvarn queue resume
```

Dispatch restarts immediately, including for anything requeued on the way in.

The state lives in the process, so a restart clears it: a host that boots comes
up ready for work. If you need it to come up drained, drain it again once it is
listening.

## Who may do it

Draining acts on the orchestrator rather than on a project, so it needs the
`host` capability — which a `--projects '*'` key does **not** carry. See
[Manage API keys](manage-api-keys.md#capabilities).

On the orchestrator's own host nothing extra is needed: the
[local control socket](run-the-orchestrator.md#the-host-local-control-socket)
is used automatically and authenticates by filesystem permissions, so a stop
script running as the service account needs no key.

```sh
# On the host itself.
kvarn queue drain --wait

# From elsewhere.
kvarn queue drain --wait --addr https://kvarn.internal --api-key kvarn_…
```

## What a plain stop still does

`SIGTERM` (or `systemctl stop`) without a drain first behaves as it always has:
the listener closes, in-flight jobs are cancelled, and each one tears its VM
down on the way out, bounded by 30 seconds. The orchestrator marks itself
draining as it goes, so nothing new starts during that window, but jobs that
were running are stopped rather than finished.

Sessions left behind are settled on the next boot: those that had not started
running return to the backlog, the rest are failed. Nothing is lost that was not
already lost with the VM — which is exactly what draining first avoids.
