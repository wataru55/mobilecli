package devices

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/mobile-next/mobilecli/utils"
)

// Device-scoped discovery of a simulator app's Dart VM service.
//
// An mDNS lookup keys only on the bundle id, and a simulator
// app runs natively on the Mac, so every booted simulator running the same
// bundle advertises the same instance name on the same 127.0.0.1. With an
// iPhone and an iPad both running the app, `dump ui --device <iPhone>` can
// therefore return the iPad's render tree — wrong device, no error.
//
// The app process itself carries the device identity: its executable lives
// under .../Devices/<UDID>/data/Containers/Bundle/Application/..., which
// simctl hands us via get_app_container. Resolving pid -> listening port from
// that path is unambiguous, and costs a `ps` plus an `lsof` (tens of ms)
// instead of the 3s mDNS timeout.

// simulatorVMServiceURI returns the Dart VM service URI of bundleID running on
// this specific simulator, or "" when it cannot be determined.
func (s *SimulatorDevice) simulatorVMServiceURI(bundleID string) string {
	appPath := s.appContainerPath(bundleID)
	if appPath == "" {
		return ""
	}

	pid := pidForExecutableUnder(appPath)
	if pid == "" {
		utils.Verbose("flutter: no running process under %s", appPath)
		return ""
	}

	for _, port := range listeningPortsForPID(pid) {
		switch probeDartVMService(port) {
		case vmServiceOpen:
			// Launched with --disable-service-auth-codes: the bare URI is valid.
			return fmt.Sprintf("http://127.0.0.1:%s/", port)

		case vmServiceNeedsToken:
			// mDNS carries the auth code in its TXT record; matching on the port
			// we already know keeps the answer device-correct even when several
			// simulators advertise the same bundle.
			if token := authCodeForPort(bundleID, port, time.Second); token != "" {
				return fmt.Sprintf("http://127.0.0.1:%s/%s/", port, token)
			}
			// The simulator log keeps the code the engine printed at launch.
			if token := s.authCodeFromLogForPort(port); token != "" {
				return fmt.Sprintf("http://127.0.0.1:%s/%s/", port, token)
			}
			// Without the token the URI would be rejected by the VM service, so
			// returning it anyway would only report a success that cannot work.
			utils.Verbose("flutter: VM service on port %s requires an auth code; neither mDNS nor the simulator log had one", port)
		}
	}
	return ""
}

// appContainerPath returns the installed .app bundle path for bundleID on this
// simulator, or "" if the app is not installed.
func (s *SimulatorDevice) appContainerPath(bundleID string) string {
	out, err := runSimctl("get_app_container", s.UDID, bundleID, "app")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// pidForExecutableUnder returns the pid of the process whose executable lives
// inside appPath. Matching on the container path rather than the process name
// keeps sibling processes of the same simulator (the DeviceKit UITests runner,
// for one) from being mistaken for the app.
func pidForExecutableUnder(appPath string) string {
	out, err := exec.Command("ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return ""
	}
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		pid, command, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(command), appPath+"/") {
			return pid
		}
	}
	return ""
}

// listeningPortsForPID returns the loopback TCP ports pid listens on.
func listeningPortsForPID(pid string) []string {
	out, err := exec.Command("lsof", "-nP", "-a", "-p", pid, "-iTCP", "-sTCP:LISTEN").Output()
	if err != nil {
		return nil
	}
	var ports []string
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		// NAME column, e.g. "127.0.0.1:57452 (LISTEN)"
		for _, f := range fields {
			host, port, err := net.SplitHostPort(strings.TrimSuffix(f, " (LISTEN)"))
			if err != nil || host != "127.0.0.1" {
				continue
			}
			ports = append(ports, port)
		}
	}
	return ports
}

// vmServiceProbe is what a plain GET on a candidate port tells us.
type vmServiceProbe int

const (
	// notVMService: the port belongs to something else entirely.
	notVMService vmServiceProbe = iota
	// vmServiceOpen: a Dart VM service that accepts requests without a token,
	// i.e. the app was launched with --disable-service-auth-codes.
	vmServiceOpen
	// vmServiceNeedsToken: a Dart VM service that rejects untokenized requests,
	// so a URI is only usable with its auth code.
	vmServiceNeedsToken
)

// probeDartVMService classifies port by how it answers a plain GET. The replies
// are distinctive: "missing or invalid authentication code" when auth codes are
// on, and either a JSON-RPC error or a note about no registered Dart
// Development Service when they are off. An app's other listeners answer
// none of these.
func probeDartVMService(port string) vmServiceProbe {
	client := &http.Client{Timeout: 700 * time.Millisecond}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%s/", port))
	if err != nil {
		return notVMService
	}
	defer func() { _ = resp.Body.Close() }()

	buf := make([]byte, 512)
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])

	if strings.Contains(body, "authentication code") {
		return vmServiceNeedsToken
	}
	for _, marker := range []string{"jsonrpc", "Dart Development Service", "Dart VM"} {
		if strings.Contains(body, marker) {
			return vmServiceOpen
		}
	}
	return notVMService
}

// authCodeForPort resolves the app's mDNS TXT record and returns its auth code
// only when the advertised port is the one we already located, so a second
// simulator advertising the same bundle cannot supply a mismatched token.
func authCodeForPort(bundleID, port string, timeout time.Duration) string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/usr/bin/dns-sd", "-L", bundleID, "_dartVmService._tcp", "local.")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return ""
	}
	if err := cmd.Start(); err != nil {
		return ""
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	var advertisedPort, auth string
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		if m := mdnsPortLine.FindStringSubmatch(line); m != nil {
			advertisedPort = m[1]
		}
		if m := mdnsAuthCode.FindStringSubmatch(line); m != nil {
			auth = m[1]
		}
		if advertisedPort != "" && auth != "" {
			if advertisedPort == port {
				return auth
			}
			advertisedPort, auth = "", ""
		}
	}
	return ""
}
