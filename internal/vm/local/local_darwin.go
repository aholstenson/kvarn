//go:build darwin

package local

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/Code-Hex/vz/v3"
	"github.com/aholstenson/kvarn/internal/egress/link"
	egressproxy "github.com/aholstenson/kvarn/internal/egress/proxy"
	"github.com/aholstenson/kvarn/internal/project"
	"github.com/aholstenson/kvarn/internal/vm"
	"github.com/aholstenson/kvarn/internal/vm/disk"
	"github.com/cockroachdb/errors"
	"gvisor.dev/gvisor/pkg/tcpip"
)

const vsockPort = 1024

type Provider struct {
	mu  sync.Mutex
	vms map[string]*vmInstance
}

type vmInstance struct {
	machine     *vz.VirtualMachine
	tmpDisk     string
	tmpSeed     string
	nvramPath   string
	serialFiles []*os.File // keep serial port file handles alive for VM lifetime
	netCancel   context.CancelFunc
	netFiles    []*os.File
	network     *link.Network
}

func (p *Provider) Name() string { return "local" }

func (p *Provider) PrepareImage(_ context.Context, base vm.BaseImage) (*vm.ProviderImage, error) {
	// Local provider uses the raw files directly — no conversion needed.
	return &vm.ProviderImage{Base: &base}, nil
}

func (p *Provider) Create(ctx context.Context, opts vm.CreateOpts) (*vm.VM, *vm.RunnerConn, error) {
	if opts.Image == nil || opts.Image.Base == nil {
		return nil, nil, errors.New("prepared image with base paths is required")
	}

	base := opts.Image.Base
	if base.DiskImagePath == "" {
		return nil, nil, errors.New("disk image path is required")
	}

	log := slog.With("disk", base.DiskImagePath)

	// Track temp files for cleanup on failure.
	var tmpDisk, tmpSeed, nvramPath string
	var serialFiles []*os.File
	var netFiles []*os.File
	var network *link.Network
	var netCancel context.CancelFunc
	success := false
	defer func() {
		if success {
			return
		}
		for _, f := range serialFiles {
			f.Close()
		}
		for _, f := range netFiles {
			f.Close()
		}
		if netCancel != nil {
			netCancel()
		}
		if network != nil {
			network.Close()
		}
		for _, p := range []string{tmpDisk, tmpSeed, nvramPath} {
			if p == "" {
				continue
			}
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				slog.Warn("failed to remove temp file", "path", p, "error", err)
			}
		}
	}()

	// Verify disk image exists.
	info, err := os.Stat(base.DiskImagePath)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "disk image path %q", base.DiskImagePath)
	}
	log.Info("image file", "file", "disk", "size", info.Size())

	// Convert qcow2 to raw in a temp file for this VM instance.
	tmpDiskFile, err := os.CreateTemp("", "kvarn-disk-*.img")
	if err != nil {
		return nil, nil, errors.Wrap(err, "create temp disk file")
	}
	tmpDisk = tmpDiskFile.Name()
	tmpDiskFile.Close()
	if err := disk.ConvertQcow2ToRaw(base.DiskImagePath, tmpDisk); err != nil {
		return nil, nil, errors.Wrap(err, "convert disk image")
	}

	// Resize disk to requested size (or default).
	diskSize := opts.DiskSizeBytes
	if diskSize == 0 {
		diskSize = project.DefaultDiskSize
	}
	if err := disk.ResizeDisk(tmpDisk, diskSize); err != nil {
		return nil, nil, errors.Wrap(err, "resize disk")
	}
	token := opts.Token

	// Generate per-VM CA so the proxy can MITM TLS, and bake the public
	// certificate into the cloud-init seed for the in-VM trust store.
	ca, err := egressproxy.GenerateCA()
	if err != nil {
		return nil, nil, errors.Wrap(err, "generate proxy CA")
	}

	// Create cloud-init seed disk with per-VM token, vsock port, and CA.
	tmpSeed = tmpDisk + ".cidata.iso"
	if err := disk.CreateCloudInitDisk(tmpSeed, disk.CloudInitOpts{
		Token:     token,
		VsockPort: vsockPort,
		ProxyCA:   ca.CertPEM(),
	}); err != nil {
		return nil, nil, errors.Wrap(err, "create cloud-init seed disk")
	}

	// Create NVRAM for EFI variable store.
	nvramPath = tmpDisk + ".nvram"
	efiStore, err := vz.NewEFIVariableStore(nvramPath, vz.WithCreatingEFIVariableStore())
	if err != nil {
		return nil, nil, errors.Wrap(err, "create EFI variable store")
	}

	bootLoader, err := vz.NewEFIBootLoader(vz.WithEFIVariableStore(efiStore))
	if err != nil {
		return nil, nil, errors.Wrap(err, "create EFI boot loader")
	}

	cpus := uint(opts.CPUs)
	if cpus == 0 {
		cpus = project.DefaultCPUs
	}
	memory := uint64(opts.MemoryBytes)
	if memory == 0 {
		memory = project.DefaultMemory
	}

	config, err := vz.NewVirtualMachineConfiguration(bootLoader, cpus, memory)
	if err != nil {
		return nil, nil, errors.Wrap(err, "create VM config")
	}

	// Disk attachment — use cached mode to avoid disk corruption on ARM Macs
	diskAttachment, err := vz.NewDiskImageStorageDeviceAttachmentWithCacheAndSync(tmpDisk, false, vz.DiskImageCachingModeCached, vz.DiskImageSynchronizationModeFsync)
	if err != nil {
		return nil, nil, errors.Wrap(err, "create disk attachment")
	}
	blockDevice, err := vz.NewVirtioBlockDeviceConfiguration(diskAttachment)
	if err != nil {
		return nil, nil, errors.Wrap(err, "create block device")
	}

	// Cloud-init seed disk (read-only).
	seedAttachment, err := vz.NewDiskImageStorageDeviceAttachment(tmpSeed, true)
	if err != nil {
		return nil, nil, errors.Wrap(err, "create seed disk attachment")
	}
	seedDevice, err := vz.NewVirtioBlockDeviceConfiguration(seedAttachment)
	if err != nil {
		return nil, nil, errors.Wrap(err, "create seed block device")
	}

	config.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{blockDevice, seedDevice})

	// Userspace network: a SOCK_DGRAM socketpair carries raw ethernet
	// frames between vz and our gvisor netstack. The orchestrator owns
	// the entire L3 fabric the VM sees, so the egress proxy is the only
	// reachable network endpoint.
	hostFile, vmFile, err := link.CreateSocketPair()
	if err != nil {
		return nil, nil, errors.Wrap(err, "create network socket pair")
	}
	netFiles = []*os.File{hostFile, vmFile}

	netAttachment, err := vz.NewFileHandleNetworkDeviceAttachment(vmFile)
	if err != nil {
		return nil, nil, errors.Wrap(err, "create file handle attachment")
	}
	networkDevice, err := vz.NewVirtioNetworkDeviceConfiguration(netAttachment)
	if err != nil {
		return nil, nil, errors.Wrap(err, "create network device")
	}
	config.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{networkDevice})

	// Bring up the userspace netstack and proxy so they are ready before
	// the VM starts emitting frames.
	rw := link.NewSocketPairFrameRW(hostFile)
	gatewayMAC := tcpip.LinkAddress("\x02\x00\x00\x00\x00\x01")
	ethEndpoint := link.NewEthernetEndpoint(rw, gatewayMAC, "", link.DefaultMTU)

	network, err = link.New(link.Config{Endpoint: ethEndpoint})
	if err != nil {
		return nil, nil, errors.Wrap(err, "create userspace network")
	}

	netCtx, cancel := context.WithCancel(context.Background())
	netCancel = cancel
	go ethEndpoint.Run(netCtx)
	go func() { _ = network.Run(netCtx) }()

	if err := startProxy(netCtx, network, ca, opts.Network); err != nil {
		return nil, nil, errors.Wrap(err, "start egress proxy")
	}

	// Vsock device for runner communication.
	vsockDevice, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return nil, nil, errors.Wrap(err, "create vsock device")
	}
	config.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{vsockDevice})

	// Serial console — always attached so boot diagnostics are available.
	{
		devNull, err := os.Open(os.DevNull)
		if err != nil {
			return nil, nil, errors.Wrap(err, "open devnull")
		}

		pr, pw, err := os.Pipe()
		if err != nil {
			devNull.Close()
			return nil, nil, errors.Wrap(err, "create serial pipe")
		}
		serialFiles = []*os.File{devNull, pr, pw}

		serialAttachment, err := vz.NewFileHandleSerialPortAttachment(devNull, pw)
		if err != nil {
			return nil, nil, errors.Wrap(err, "create serial attachment")
		}
		consoleConfig, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(serialAttachment)
		if err != nil {
			return nil, nil, errors.Wrap(err, "create console config")
		}
		config.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{consoleConfig})

		go func() {
			scanner := bufio.NewScanner(pr)
			for scanner.Scan() {
				line := scanner.Text()
				if opts.OnConsoleOutput != nil {
					opts.OnConsoleOutput(line + "\n")
				}
			}
			if err := scanner.Err(); err != nil && !errors.Is(err, io.ErrClosedPipe) {
				slog.Debug("serial console reader stopped", "error", err)
			}
		}()
	}

	valid, err := config.Validate()
	if err != nil || !valid {
		return nil, nil, errors.Wrap(err, "invalid VM config")
	}

	log.Info("VM config validated, creating VM", "cpus", cpus, "memory", memory, "diskSize", diskSize)

	machine, err := vz.NewVirtualMachine(config)
	if err != nil {
		return nil, nil, errors.Wrap(err, "create VM")
	}

	log.Info("VM created", "state", machine.State(), "canStart", machine.CanStart())

	// Watch for state changes in the background for diagnostics.
	go func() {
		for state := range machine.StateChangedNotify() {
			log.Info("VM state changed", "state", state)
		}
	}()

	if err := machine.Start(); err != nil {
		log.Error("VM failed to start", "state", machine.State(), "error", err)
		return nil, nil, errors.Wrap(err, "start VM")
	}

	log.Info("VM started", "state", machine.State())

	// Listen on vsock for the runner to connect.
	socketDevices := machine.SocketDevices()
	if len(socketDevices) == 0 {
		machine.Stop()
		return nil, nil, errors.New("no socket devices found on VM")
	}

	listener, err := socketDevices[0].Listen(vsockPort)
	if err != nil {
		machine.Stop()
		return nil, nil, errors.Wrap(err, "vsock listen")
	}

	id := fmt.Sprintf("local-%d", time.Now().UnixNano())

	p.mu.Lock()
	if p.vms == nil {
		p.vms = make(map[string]*vmInstance)
	}
	p.vms[id] = &vmInstance{
		machine:     machine,
		tmpDisk:     tmpDisk,
		tmpSeed:     tmpSeed,
		nvramPath:   nvramPath,
		serialFiles: serialFiles,
		netCancel:   netCancel,
		netFiles:    netFiles,
		network:     network,
	}
	p.mu.Unlock()

	slog.Info("local VM started", "vm", id)

	success = true
	return &vm.VM{
			ID:    id,
			Token: token,
		}, &vm.RunnerConn{
			Listener: listener,
		}, nil
}

func (p *Provider) Destroy(_ context.Context, id string) error {
	p.mu.Lock()
	inst, ok := p.vms[id]
	if ok {
		delete(p.vms, id)
	}
	p.mu.Unlock()

	if !ok {
		return errors.Newf("VM %s not found", id)
	}

	if err := inst.machine.Stop(); err != nil {
		slog.Warn("failed to stop VM", "vm", id, "error", err)
	}

	if inst.netCancel != nil {
		inst.netCancel()
	}
	if inst.network != nil {
		inst.network.Close()
	}
	for _, f := range inst.netFiles {
		f.Close()
	}
	for _, f := range inst.serialFiles {
		f.Close()
	}
	os.Remove(inst.tmpDisk)
	os.Remove(inst.tmpSeed)
	os.Remove(inst.nvramPath)
	slog.Info("local VM destroyed", "vm", id)
	return nil
}

func (p *Provider) List(_ context.Context) ([]*vm.VM, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var vms []*vm.VM
	for id := range p.vms {
		vms = append(vms, &vm.VM{
			ID: id,
		})
	}
	return vms, nil
}
