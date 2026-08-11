package sandbox

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
)

// caCertificateDir is Debian's drop-in directory for locally added trust
// anchors. update-ca-certificates folds every certificate here into
// /etc/ssl/certs/ca-certificates.crt, the bundle the guest's TLS clients
// resolve to.
const caCertificateDir = "/usr/local/share/ca-certificates"

// nssDBDir is the NSS database Chromium reads trust from. NSS clients ignore
// /etc/ssl/certs entirely, so a browser rejects every proxied connection as
// ERR_CERT_AUTHORITY_INVALID unless the CA is also in here.
//
// Chromium prefers $HOME/.pki/nssdb when it exists and only falls back to the
// XDG location ($XDG_DATA_HOME/pki/nssdb) otherwise, so creating this path
// covers both the old and the new lookup.
const nssDBDir = kvarnHome + "/.pki/nssdb"

// nssCertNickname is what the CA is called inside the NSS database, which
// keys certificates by nickname rather than by subject.
const nssCertNickname = "kvarn-egress-proxy"

// proxyCATimeoutSeconds caps each trust-store update. The slower of the two
// rehashes a few hundred certificates off local disk, so anything near this
// bound means the guest is wedged rather than busy.
const proxyCATimeoutSeconds uint32 = 60

// InstallProxyCA adds the per-VM egress proxy CA to the guest trust stores:
// the system anchor directory, whose combined bundle is what OpenSSL, curl,
// git and Python resolve to, and the job user's NSS database, which is the
// only one Chromium consults. A VM whose provider does not MITM TLS passes an
// empty caPEM and installs nothing.
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

	installNSSTrust(ctx, runner)
	return nil
}

// installNSSTrust adds the proxy CA to the job user's NSS database, which is
// where Chromium and every other NSS client look instead of at the combined
// bundle above.
//
// It runs unprivileged so that su -l puts the database under the job user's
// own home and ownership; certutil creates the database on first -A, so no
// separate init step is needed.
//
// A failure here is logged rather than returned. Only NSS clients depend on
// it, the image ABI kvarn accepts spans versions that predate certutil being
// installed, and failing the job would take out every job that never opens a
// browser.
func installNSSTrust(ctx context.Context, runner RunnerProxy) {
	cmd := fmt.Sprintf(`mkdir -p %s && certutil -d sql:%s -A -t "C,," -n %s -i %s/kvarn-proxy.crt`,
		nssDBDir, nssDBDir, nssCertNickname, caCertificateDir)

	resp, err := runner.Exec(ctx, &v1.ExecRequest{
		Command:        cmd,
		TimeoutSeconds: proxyCATimeoutSeconds,
	})
	switch {
	case err != nil:
		slog.Warn("could not add proxy CA to the NSS trust store; browsers in this VM will reject proxied TLS",
			"error", err)
	case resp.ExitCode != 0:
		slog.Warn("could not add proxy CA to the NSS trust store; browsers in this VM will reject proxied TLS",
			"exit_code", resp.ExitCode, "stderr", strings.TrimSpace(resp.Stderr))
	}
}
