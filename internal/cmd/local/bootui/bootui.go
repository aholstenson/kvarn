// Package bootui renders a local sandbox boot onto the task UI.
//
// Every `kvarn local` command boots the same sandbox and shows the same
// provisioning, dependency and step output while it happens. Keeping that
// rendering here is what makes `local job`, `local test` and `local preview`
// look like one tool rather than three that happen to share a VM.
package bootui

import (
	"fmt"

	"github.com/aholstenson/kvarn/internal/linebuf"
	"github.com/aholstenson/kvarn/internal/project"
	"github.com/aholstenson/kvarn/internal/sandbox"
	"github.com/aholstenson/kvarn/internal/taskui"
)

// Boot turns sandbox boot events into task-UI items.
//
// Provisioning is reported as a sequence of phases rather than a fixed list,
// because which ones happen depends on the project and on what the cache
// already holds. Each phase therefore completes implicitly when the next one
// starts, and the caller closes the last one with Done once the boot returns.
type Boot struct {
	renderer *taskui.Renderer

	last         *taskui.Item
	dependencies *taskui.Item
	tool         *taskui.Item
}

// New creates a boot renderer writing to r.
func New(r *taskui.Renderer) *Boot { return &Boot{renderer: r} }

// Done completes whichever provisioning phase is still open. It is safe to call
// more than once.
func (b *Boot) Done() {
	if b.last != nil {
		b.renderer.SetStatus(b.last, taskui.StatusPassed, "")
		b.last = nil
	}
}

// begin closes the running phase and opens a new one.
func (b *Boot) begin(name string) *taskui.Item {
	b.Done()
	item := b.renderer.AddItem(name)
	b.renderer.SetStatus(item, taskui.StatusRunning, "")
	b.last = item
	return item
}

// OnEvent satisfies sandbox.Opts.OnEvent.
func (b *Boot) OnEvent(e sandbox.Event) {
	switch ev := e.(type) {
	case sandbox.ProvisioningEvent:
		b.begin("Provisioning VM")
	case sandbox.TransferringEvent:
		b.begin("Transferring files")
	case sandbox.TransferProgressEvent:
		if b.last != nil {
			b.last.Suffix = fmt.Sprintf("%.1f MB / %.1f MB",
				float64(ev.BytesSent)/(1024*1024),
				float64(ev.TotalBytes)/(1024*1024))
		}
	case sandbox.DependenciesInstallingEvent:
		b.dependencies = b.begin("Installing dependencies")
	case sandbox.DependencyOutputEvent:
		if b.dependencies != nil {
			b.renderer.AppendStreams(b.dependencies, ev.Stdout, ev.Stderr)
		}
	case sandbox.DependenciesInstalledEvent:
		if b.dependencies != nil {
			b.renderer.SetStatus(b.dependencies, taskui.StatusPassed, "")
			if b.last == b.dependencies {
				b.last = nil
			}
			b.dependencies = nil
		}
	case sandbox.ToolProvisioningEvent:
		b.tool = b.begin(fmt.Sprintf("Provisioning %s", ev.Tool))
	case sandbox.ToolProvisionOutputEvent:
		if b.tool != nil {
			b.renderer.AppendStreams(b.tool, ev.Stdout, ev.Stderr)
		}
	case sandbox.ToolProvisionedEvent:
		if b.tool != nil {
			b.renderer.SetStatus(b.tool, taskui.StatusPassed, "")
			if b.last == b.tool {
				b.last = nil
			}
			b.tool = nil
		}
	case sandbox.EgressDeniedEvent:
		// Shown against whatever provisioning step is running, which is where a
		// blocked download usually surfaces. Denials outside one still reach the
		// failure message via the session's host list.
		if b.last != nil {
			b.renderer.AppendOutput(b.last, fmt.Sprintf("egress denied: %s", ev.Host))
		}
	case sandbox.CacheRestoringEvent:
		b.begin("Restoring cache")
	case sandbox.CacheProgressEvent:
		if b.last != nil {
			b.last.Suffix = fmt.Sprintf("%s (%d/%d)", ev.Path, ev.Index, ev.Total)
		}
	case sandbox.CacheRestoredEvent:
		b.Done()
	case sandbox.CacheSavingEvent:
		b.begin("Saving cache")
	case sandbox.CacheSavedEvent:
		b.Done()
	case sandbox.SessionCreatingEvent:
		b.begin("Creating shell session")
	case sandbox.SessionCreatedEvent:
		b.Done()
	}
}

// StepCallbacks builds the step callbacks for one group of steps, adding each
// as a child of parent. Every step is pre-populated in pending state so what is
// coming is visible before it runs, and onDone is called once per step after
// its item has been given its final status, for whatever accounting the caller
// keeps.
func StepCallbacks(
	r *taskui.Renderer,
	parent *taskui.Item,
	steps []project.Step,
	onDone func(sandbox.StepResult),
) (sandbox.OnStepDone, sandbox.OnOutput) {
	childItems := make(map[string]*taskui.Item, len(steps))
	stdoutBufs := make(map[string]*linebuf.Buffer, len(steps))
	stderrBufs := make(map[string]*linebuf.Buffer, len(steps))
	for _, step := range steps {
		child := r.AddChild(parent, step.Name)
		child.Status = taskui.StatusPending
		childItems[step.Name] = child
		stdoutBufs[step.Name] = &linebuf.Buffer{}
		stderrBufs[step.Name] = &linebuf.Buffer{}
	}

	// A step the caller did not declare can still report — a tool step, or one
	// synthesised during the run — so both callbacks create what they are
	// missing rather than dropping the output.
	itemFor := func(name string) *taskui.Item {
		item, ok := childItems[name]
		if !ok {
			item = r.AddChild(parent, name)
			childItems[name] = item
		}
		return item
	}
	bufFor := func(m map[string]*linebuf.Buffer, name string) *linebuf.Buffer {
		buf, ok := m[name]
		if !ok {
			buf = &linebuf.Buffer{}
			m[name] = buf
		}
		return buf
	}

	onOutput := func(stepName string, _ string, stdout string, stderr string) {
		item := itemFor(stepName)
		if item.Status == taskui.StatusPending {
			r.SetStatus(item, taskui.StatusRunning, "")
		}
		for _, line := range bufFor(stdoutBufs, stepName).Append(stdout) {
			r.AppendOutput(item, line)
		}
		for _, line := range bufFor(stderrBufs, stepName).Append(stderr) {
			r.AppendOutput(item, line)
		}
	}

	onStepDone := func(result sandbox.StepResult, _ string) {
		item := itemFor(result.Name)

		// Flush any partial trailing line so an unterminated final chunk still
		// appears in the live view.
		if tail := bufFor(stdoutBufs, result.Name).Flush(); tail != "" {
			r.AppendOutput(item, tail)
		}
		if tail := bufFor(stderrBufs, result.Name).Flush(); tail != "" {
			r.AppendOutput(item, tail)
		}

		switch {
		case result.Skipped:
			r.SetStatus(item, taskui.StatusSkipped, "(no matching files)")
		case result.ExitCode != 0 || result.Err != nil:
			r.SetStatus(item, taskui.StatusFailed, "")
		default:
			r.SetStatus(item, taskui.StatusPassed, "")
		}

		delete(childItems, result.Name)
		delete(stdoutBufs, result.Name)
		delete(stderrBufs, result.Name)

		if onDone != nil {
			onDone(result)
		}
	}

	return onStepDone, onOutput
}

// ParentStatus is the status a group takes from its children: failed if any
// child failed, passed otherwise.
func ParentStatus(parent *taskui.Item) taskui.Status {
	for _, child := range parent.Children {
		if child.Status == taskui.StatusFailed {
			return taskui.StatusFailed
		}
	}
	return taskui.StatusPassed
}
