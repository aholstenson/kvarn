package link

import (
	"context"
	"sync"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// FrameRW exchanges raw ethernet frames with the VM. Each ReadFrame call
// returns a single frame including its 14-byte ethernet header. WriteFrame
// is symmetric. Implementations are concrete per hypervisor: vz on darwin,
// qemu on linux.
type FrameRW interface {
	ReadFrame() ([]byte, error)
	WriteFrame([]byte) error
	Close() error
}

// EthernetEndpoint adapts a FrameRW to a gvisor stack.LinkEndpoint by
// stripping/adding ethernet headers and dispatching the resulting L3
// packets into the stack.
type EthernetEndpoint struct {
	rw      FrameRW
	mtu     uint32
	addr    tcpip.LinkAddress // our MAC; the gateway side
	peerMAC tcpip.LinkAddress // VM's MAC; learned on first frame

	mu         sync.Mutex
	dispatcher stack.NetworkDispatcher
	onClose    func()

	closeOnce sync.Once
	closed    chan struct{}
}

// NewEthernetEndpoint wraps rw as a stack.LinkEndpoint. localMAC is the
// MAC the gateway advertises; peerMAC is the VM's MAC if known (otherwise
// it is learned from inbound frames).
func NewEthernetEndpoint(rw FrameRW, localMAC, peerMAC tcpip.LinkAddress, mtu uint32) *EthernetEndpoint {
	if mtu == 0 {
		mtu = DefaultMTU
	}
	return &EthernetEndpoint{
		rw:      rw,
		mtu:     mtu,
		addr:    localMAC,
		peerMAC: peerMAC,
		closed:  make(chan struct{}),
	}
}

// Run starts the read pump. Returns when ctx is cancelled or the
// underlying transport errors out.
func (e *EthernetEndpoint) Run(ctx context.Context) {
	go func() {
		<-ctx.Done()
		e.Close()
	}()
	for {
		frame, err := e.rw.ReadFrame()
		if err != nil {
			return
		}
		if len(frame) < header.EthernetMinimumSize {
			continue
		}

		eth := header.Ethernet(frame[:header.EthernetMinimumSize])
		proto := eth.Type()
		src := eth.SourceAddress()
		if e.peerMAC == "" {
			e.peerMAC = src
		}

		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
			Payload: buffer.MakeWithData(frame[header.EthernetMinimumSize:]),
		})

		e.mu.Lock()
		d := e.dispatcher
		e.mu.Unlock()
		if d != nil {
			d.DeliverNetworkPacket(proto, pkt)
		}
		pkt.DecRef()
	}
}

// Close releases resources. Safe to call concurrently and repeatedly.
func (e *EthernetEndpoint) Close() {
	e.closeOnce.Do(func() {
		e.mu.Lock()
		cb := e.onClose
		e.mu.Unlock()
		if cb != nil {
			cb()
		}
		close(e.closed)
		_ = e.rw.Close()
	})
}

// stack.LinkEndpoint implementation

func (e *EthernetEndpoint) MTU() uint32       { return e.mtu }
func (e *EthernetEndpoint) SetMTU(mtu uint32) { e.mtu = mtu }

func (e *EthernetEndpoint) MaxHeaderLength() uint16 { return header.EthernetMinimumSize }

func (e *EthernetEndpoint) LinkAddress() tcpip.LinkAddress     { return e.addr }
func (e *EthernetEndpoint) SetLinkAddress(a tcpip.LinkAddress) { e.addr = a }

func (e *EthernetEndpoint) Capabilities() stack.LinkEndpointCapabilities {
	return stack.CapabilityResolutionRequired
}

func (e *EthernetEndpoint) Attach(d stack.NetworkDispatcher) {
	e.mu.Lock()
	e.dispatcher = d
	e.mu.Unlock()
}

func (e *EthernetEndpoint) IsAttached() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.dispatcher != nil
}

func (e *EthernetEndpoint) Wait() {
	<-e.closed
}

func (e *EthernetEndpoint) SetOnCloseAction(f func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.onClose = f
}

func (e *EthernetEndpoint) ARPHardwareType() header.ARPHardwareType {
	return header.ARPHardwareEther
}

func (e *EthernetEndpoint) AddHeader(pkt *stack.PacketBuffer) {
	eth := header.Ethernet(pkt.LinkHeader().Push(header.EthernetMinimumSize))
	hdr := &header.EthernetFields{
		SrcAddr: e.addr,
		DstAddr: e.peerMAC,
		Type:    pkt.NetworkProtocolNumber,
	}
	eth.Encode(hdr)
}

func (e *EthernetEndpoint) ParseHeader(pkt *stack.PacketBuffer) bool {
	_, ok := pkt.LinkHeader().Consume(header.EthernetMinimumSize)
	return ok
}

func (e *EthernetEndpoint) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error) {
	written := 0
	for _, pkt := range pkts.AsSlice() {
		// The packet already has a link header pushed by AddHeader (called
		// by the stack via a NIC's link.write path). Concatenate slices
		// and send as a single ethernet frame.
		buf := make([]byte, 0, pkt.Size())
		for _, v := range pkt.AsSlices() {
			buf = append(buf, v...)
		}
		if err := e.rw.WriteFrame(buf); err != nil {
			return written, &tcpip.ErrAborted{}
		}
		written++
	}
	return written, nil
}

func (e *EthernetEndpoint) SupportedGSO() stack.SupportedGSO { return stack.GSONotSupported }
