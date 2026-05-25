package liveness

import (
	"os"
	"testing"
)

// TestCaptureAndAlive exercises the full round trip: Capture fingerprints the
// process tree above the test binary (its parent is `go test`, not a shell), and
// that process is obviously still running.
func TestCaptureAndAlive(t *testing.T) {
	id, ok := Capture()
	if !ok {
		t.Fatal("Capture returned ok=false; expected to fingerprint a parent process")
	}
	if id.PID <= 0 || id.BootID == "" {
		t.Fatalf("captured incomplete identity: %+v", id)
	}
	if !Alive(id) {
		t.Error("Alive(captured)=false; the captured process should still be running")
	}
}

// TestAliveDetectsDeath builds a known-good identity for the test process itself,
// then checks that each way a process can vanish is reported as dead.
func TestAliveDetectsDeath(t *testing.T) {
	boot, err := bootID()
	if err != nil {
		t.Fatal(err)
	}
	self := os.Getpid()
	start, err := startTime(self)
	if err != nil {
		t.Fatal(err)
	}
	if !Alive(Identity{PID: self, Start: start, BootID: boot}) {
		t.Fatal("Alive(self)=false; the test process is running")
	}

	dead := map[string]Identity{
		"pid reused (start mismatch)": {PID: self, Start: start + 1, BootID: boot},
		"reboot (boot mismatch)":      {PID: self, Start: start, BootID: "00000000-0000-0000-0000-000000000000"},
		"no such pid":                 {PID: 0x7fffffff, Start: start, BootID: boot},
		"zero identity":               {},
	}
	for name, id := range dead {
		if Alive(id) {
			t.Errorf("%s: Alive=true, want false", name)
		}
	}
}
