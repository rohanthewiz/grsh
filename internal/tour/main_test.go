package tour

import (
	"os"
	"testing"
)

// TestMain lets the test binary be a worker.
//
// A worker is this executable started again with WorkerEnv set (see
// worker.go), and under `go test` "this executable" is the test binary.
// Without this hook every test would have to opt out of the real
// architecture with Options.InProcess, and the protocol that carries a
// student's whole session — output ordering, interrupts, progress,
// shutdown — would have no test at all.
//
// The dispatch has to happen before m.Run: a worker is not running tests,
// it is running one student's shell, and it must never print a test
// summary onto a terminal that belongs to a tour.
func TestMain(m *testing.M) {
	if IsWorker() {
		os.Exit(RunWorkerProcess())
	}
	os.Exit(m.Run())
}
