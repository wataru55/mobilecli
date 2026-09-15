package devices

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/mobile-next/mobilecli/types"
	"github.com/mobile-next/mobilecli/utils"
)

// Flutter support for the iOS simulator. The render-tree walk is identical Dart
// to Android (same `_ReusableRenderView` root, same `invoke`/localToGlobal), so
// the whole flutterVM client and dumpRenderTree are reused verbatim. Only two
// things are iOS-specific: detecting Flutter (the app bundle embeds
// Flutter.framework) and obtaining the Dart VM service URI. On the simulator the
// app runs natively on the Mac, so its VM service listens on the Mac's own
// 127.0.0.1 — we connect directly, no port forwarding and no code injection.
//
// iOS reports layout in logical points (matching the existing DeviceKit dump),
// so unlike Android we do not scale by the device pixel ratio (dpr = 1.0).

// vmServiceLineURL pulls the port and auth token out of the engine's log line
// "The Dart VM service is listening on http://127.0.0.1:PORT/TOKEN/".
var vmServiceLineURL = regexp.MustCompile(`http://127\.0\.0\.1:(\d+)/([A-Za-z0-9_=-]*)/`)

// tryDumpFlutterSource returns the Flutter render tree for the foreground app,
// or ok=false to signal the caller should use the accessibility dump.
func (s *SimulatorDevice) tryDumpFlutterSource() ([]types.ScreenElement, bool) {
	foreground, err := s.GetForegroundApp()
	if err != nil {
		return nil, false
	}
	bundleID := foreground.PackageName
	if !s.isFlutterAppBundle(bundleID) {
		return nil, false
	}
	uri := s.flutterVMServiceURI(bundleID)
	if uri == "" {
		utils.Verbose("flutter: no Dart VM service URI found for %s (release build, or the launch log rotated out)", bundleID)
		return nil, false
	}
	start := time.Now()
	elements, err := dumpFlutterSourceFromURI(uri, 1.0)
	if err != nil {
		utils.Verbose("flutter: render-tree dump failed, falling back: %v", err)
		return nil, false
	}
	utils.Verbose("flutter: render-tree dump produced %d elements in %s", len(elements), time.Since(start))
	return elements, true
}

// isFlutterAppBundle reports whether the installed app embeds Flutter.framework.
func (s *SimulatorDevice) isFlutterAppBundle(bundleID string) bool {
	out, err := runSimctl("get_app_container", s.UDID, bundleID, "app")
	if err != nil {
		return false
	}
	appPath := strings.TrimSpace(string(out))
	if appPath == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(appPath, "Frameworks", "Flutter.framework"))
	return err == nil && info.IsDir()
}

// flutterVMServiceURI recovers the running app's Dart VM service URI (with its
// auth token). Everything here hangs off the target app's own process (see
// simulator_flutter_process.go), whose executable path carries the UDID and
// whose listening port identifies the VM service.
//
// Neither of the looser sources can stand on its own. A host-wide mDNS lookup
// keys only on the bundle id, and a simulator app runs natively on the Mac, so
// it cannot tell two simulators running the same app apart. The simulator log
// is device-scoped but holds a line for every Flutter app launched there, so
// its newest entry may belong to a different app or to a dead earlier run.
// Both are therefore used only to supply the auth code for the port we already
// resolved, never to choose the port. Answering for the wrong app or device is
// worse than not answering at all.
func (s *SimulatorDevice) flutterVMServiceURI(bundleID string) string {
	return s.simulatorVMServiceURI(bundleID)
}

var (
	mdnsPortLine = regexp.MustCompile(`can be reached at \S+?:(\d+)`)
	mdnsAuthCode = regexp.MustCompile(`authCode=([A-Za-z0-9_=+/\-]+)`)
)

// authCodeFromLogForPort returns the auth code the engine printed at launch for
// the VM service listening on port, or "" when the log no longer carries it.
//
// The log is device-scoped but not app-scoped, so the port is what ties a line
// to the right app: it comes from the target app's own process, and lines for
// other apps or for dead earlier runs carry different ports. Matching on it is
// what makes this safe to use. Fragile in the other direction too — the line
// rotates out of the buffer over time — so an empty result is normal.
func (s *SimulatorDevice) authCodeFromLogForPort(port string) string {
	out, err := runSimctl("spawn", s.UDID, "log", "show", "--last", "30m",
		"--style", "compact", "--predicate", `eventMessage CONTAINS "Dart VM service is listening"`)
	if err != nil {
		utils.Verbose("flutter: reading simulator log failed: %v", err)
		return ""
	}
	token := ""
	for _, m := range vmServiceLineURL.FindAllStringSubmatch(string(out), -1) {
		if m[1] == port {
			token = m[2] // newest line for this port wins
		}
	}
	return token
}

// dumpFlutterSourceFromURI connects to a Dart VM service already reachable at
// uri's host:port (the iOS-simulator case — the app runs on the Mac) and walks
// the render tree. dpr is 1.0 for iOS (points) and the device pixel ratio for
// Android (physical pixels).
func dumpFlutterSourceFromURI(uri string, dpr float64) ([]types.ScreenElement, error) {
	m := vmServiceURIPattern.FindStringSubmatch(strings.TrimSpace(uri))
	if m == nil {
		return nil, fmt.Errorf("unexpected Dart VM service URI: %q", uri)
	}
	// m[2] is the auth token; it is empty when the app was launched with
	// --disable-service-auth-codes (the no-log variant), which changes the path.
	wsURL := fmt.Sprintf("ws://127.0.0.1:%s/ws", m[1])
	if m[2] != "" {
		wsURL = fmt.Sprintf("ws://127.0.0.1:%s/%s/ws", m[1], m[2])
	}
	return dumpFlutterTreeOverWS(wsURL, dpr)
}

// dumpFlutterTreeOverWS is the platform-neutral core: dial the Dart VM service
// WebSocket, bind the isolate, and walk the render tree. Android reaches it
// through an adb-forwarded local port; iOS connects to the Mac directly.
func dumpFlutterTreeOverWS(wsURL string, dpr float64) ([]types.ScreenElement, error) {
	vm, err := dialFlutterVM(wsURL)
	if err != nil {
		return nil, err
	}
	defer vm.close()
	if err := vm.resolveIsolate(); err != nil {
		return nil, err
	}
	return vm.dumpRenderTree(dpr)
}
