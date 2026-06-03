package vm

import (
	"context"
	"net"
	"net/http"
	"time"

	egressproxy "github.com/aholstenson/kvarn/internal/egress/proxy"
)

type VM struct {
	ID    string
	Token string
}

type RunnerConn struct {
	Listener net.Listener // non-nil for local (vsock); nil for cloud (runner connects over network)

	// ExpectedPeerCID is the vsock context-ID the guest is expected to
	// connect from. The dispatch listener wrapper rejects connections from
	// any other CID, so a second VM that learned this port cannot impersonate
	// the runner. Zero means "unknown" — the wrapper falls back to
	// trust-on-first-use, locking onto the first peer it sees.
	ExpectedPeerCID uint32
}

// BaseImage holds the canonical build artifacts produced by the image build
// pipeline. Every provider receives the same base image and converts it to
// whatever format the target platform requires.
type BaseImage struct {
	DiskImagePath string // unified EFI-bootable GPT disk image
}

// ProviderImage is the provider-specific representation of a prepared image.
// For local providers this is a no-op passthrough of the base image paths.
// For cloud providers this typically holds a cloud resource ID (e.g. AMI ID,
// GCE image name, Azure image ID) after upload/conversion.
type ProviderImage struct {
	// ID is the provider-specific image identifier. For local providers this
	// is empty; for cloud providers it is the registered image resource ID.
	ID string

	// Base is retained for providers that use the raw files directly (local).
	Base *BaseImage
}

type CreateOpts struct {
	Image            *ProviderImage
	Token            string              // one-time bootstrap token for runner registration
	OrchestratorAddr string              // for cloud providers; runner needs to know where to connect
	DiskSizeBytes    int64               // desired disk size; 0 means use default
	CPUs             uint                // desired vCPU count; 0 means use default
	MemoryBytes      uint64              // desired memory in bytes; 0 means use default
	OnConsoleOutput  func(output string) // called with serial console chunks; nil = discard

	// MaxLifetime is a failsafe: after this much wall time the provider
	// destroys the VM regardless of caller state. Zero disables the cap. Not
	// a normal control path — set it from the operator-level host config so
	// a misbehaving job can never burn an unbounded amount of host resources.
	MaxLifetime time.Duration

	// Network configures the per-VM userspace network and egress proxy.
	// Local providers create the netstack and proxy at VM-create time so
	// they share the VM's lifecycle; cloud providers may ignore this and
	// rely on platform-native egress controls.
	Network NetworkConfig
}

// NetworkConfig configures the per-VM userspace network and egress proxy.
type NetworkConfig struct {
	// AllowedHosts is the resolved superset of hosts the VM is permitted
	// to reach over HTTP/HTTPS. Wildcard entries ("*.example.com") are
	// honoured by the proxy's allowlist.
	AllowedHosts []string

	// SecretInjector enriches outbound proxied requests with credentials
	// before they are sent upstream. May be nil to forward unmodified.
	SecretInjector egressproxy.SecretInjector

	// ImageCacheHandler, when non-nil, is bound on the per-VM gateway IP
	// at ImageCachePort so the VM's container runtime can use it as a
	// pull-through OCI registry mirror. Same handler is shared across
	// every VM — the cache itself is global, content-addressed, and
	// project-agnostic.
	ImageCacheHandler http.Handler

	// ImageCachePort is the TCP port for ImageCacheHandler on the gateway
	// IP. Ignored when ImageCacheHandler is nil.
	ImageCachePort uint16

	// ImageCacheUpstreams lists the registry hostnames the cache is
	// configured to mirror. The cloud-init layer turns this list into
	// one [[registry]] block per entry pointing at the gateway:port.
	ImageCacheUpstreams []string
}

type Provider interface {
	Name() string

	// PrepareImage converts the canonical base image into the format required
	// by this provider. For local providers this is a passthrough. For cloud
	// providers this handles format conversion (e.g. raw→VHD) and upload,
	// returning an image ID that can be reused across multiple Create calls.
	PrepareImage(ctx context.Context, base BaseImage) (*ProviderImage, error)

	Create(ctx context.Context, opts CreateOpts) (*VM, *RunnerConn, error)
	Destroy(ctx context.Context, id string) error
	List(ctx context.Context) ([]*VM, error)
}
