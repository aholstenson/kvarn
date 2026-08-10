package sandbox

import (
	"context"
	"fmt"
	"strings"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
)

// caCertificateDir is Debian's drop-in directory for locally added trust
// anchors. update-ca-certificates folds every certificate here into
// /etc/ssl/certs/ca-certificates.crt, the bundle the guest's TLS clients
// resolve to.
const caCertificateDir = "/usr/local/share/ca-certificates"

// proxyCATimeoutSeconds caps the update-ca-certificates run. It rehashes a
// few hundred certificates off local disk, so anything near this bound means
// the guest is wedged rather than busy.
const proxyCATimeoutSeconds uint32 = 60

// InstallProxyCA adds the per-VM egress proxy CA to the guest trust store
// and rebuilds the combined bundle. A VM whose provider does not MITM TLS
// passes an empty caPEM and installs nothing.
//
// This is the only thing that establishes proxy trust in the guest, and it
// deliberately runs here rather than at boot. cloud-init can only install a
// trust anchor from runcmd, which executes in cloud-final — two systemd
// units after the stage that writes the runner's env file and thereby
// releases kvarn-runner.service. A boot-time install therefore races job
// start: in the gap the runner accepts commands while
// /etc/ssl/certs/ca-certificates.crt is still stock Debian, and the first
// guest command to speak TLS through the proxy — a dependency install, a
// cache restore, a container pull — rejects the proxy's certificate as
// self-signed. Driving it over the runner connection makes trust an ordered
// step instead of a likely one.
//
// Being the sole installer also matters. update-ca-certificates stages the
// new bundle at the fixed path /etc/ssl/certs/ca-certificates.crt.new and
// renames it, so two concurrent runs collide: the loser's temp file is gone
// by the time it renames, and it exits non-zero.
func InstallProxyCA(ctx context.Context, runner RunnerProxy, caPEM []byte) error {
	if len(caPEM) == 0 {
		return nil
	}

	if _, err := runner.UploadFiles(ctx, &v1.UploadFilesRequest{
		WorkingDir: caCertificateDir,
		Files: []*v1.FileContent{{
			Path:    "kvarn-proxy.crt",
			Content: caPEM,
			Mode:    0o644,
		}},
	}); err != nil {
		return fmt.Errorf("write proxy CA: %w", err)
	}

	resp, err := runner.Exec(ctx, &v1.ExecRequest{
		Command:        "update-ca-certificates",
		Privileged:     true,
		TimeoutSeconds: proxyCATimeoutSeconds,
	})
	if err != nil {
		return fmt.Errorf("exec update-ca-certificates: %w", err)
	}
	if resp.ExitCode != 0 {
		return fmt.Errorf("update-ca-certificates failed (exit %d): %s",
			resp.ExitCode, strings.TrimSpace(resp.Stderr))
	}
	return nil
}
